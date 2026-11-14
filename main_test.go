package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// A table-driven test: each row is one case, and one loop runs them all.
// Adding a case means adding one line, not writing a new test.
func TestNormalizeURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"adds slash to empty path", "https://site.com", "https://site.com/"},
		{"lowercases scheme and host only", "HTTPS://Site.COM/Page", "https://site.com/Page"},
		{"drops fragment", "https://site.com/a#top", "https://site.com/a"},
		{"drops default https port", "https://site.com:443/a", "https://site.com/a"},
		{"keeps other ports", "http://site.com:8080/a", "http://site.com:8080/a"},
		{"removes tracking params", "https://site.com/p?utm_source=x&fbclid=y&id=1", "https://site.com/p?id=1"},
		{"sorts query params", "https://site.com/p?b=2&a=1", "https://site.com/p?a=1&b=2"},
		{"drops empty query", "https://site.com/p?", "https://site.com/p"},
		{"keeps trailing slash on purpose", "https://site.com/blog/", "https://site.com/blog/"},
	}

	for _, tt := range tests {
		// t.Run gives each case its own name in the output,
		// and lets you run one case with: go test -run 'TestNormalizeURL/drops_fragment'
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.in)
			if err != nil {
				t.Fatalf("could not parse %q: %v", tt.in, err) // Fatalf stops this case
			}
			if got := normalizeURL(u); got != tt.want {
				t.Errorf("normalizeURL(%q) = %q, want %q", tt.in, got, tt.want) // Errorf records and continues
			}
		})
	}
}

func TestSameSite(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"site.com", "site.com", true},
		{"www.site.com", "site.com", true},
		{"WWW.Site.com", "site.com", true},
		{"blog.site.com", "site.com", false},
		{"other.com", "site.com", false},
		{"site.com:8080", "site.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.a+" vs "+tt.b, func(t *testing.T) {
			if got := sameSite(tt.a, tt.b); got != tt.want {
				t.Errorf("sameSite(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestParsePage(t *testing.T) {
	// A backquoted string can span many lines, which is handy for test HTML.
	html := `<html><head>
		<title>  My Page  </title>
		<meta name="description" content="About this page">
		<link rel="canonical" href="https://site.com/page">
	</head><body>
		<h1>Main heading</h1>
		<a href="/about">relative link</a>
		<a href="/about#team">same page, different fragment</a>
		<a href="https://www.site.com/contact">www version of the site</a>
		<a href="https://other.com/">external</a>
		<a href="mailto:hi@site.com">email</a>
		<a href="/cdn-cgi/l/email-protection#abc">Cloudflare email</a>
		<img src="a.png" alt="A photo">
		<img src="b.png" alt="">
		<img src="c.png">
	</body></html>`

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://site.com/page")

	r := &PageReport{}
	parsePage(doc, base, r)

	if r.Title != "My Page" {
		t.Errorf("Title = %q, want %q", r.Title, "My Page")
	}
	if r.MetaDescription != "About this page" {
		t.Errorf("MetaDescription = %q", r.MetaDescription)
	}
	if r.Canonical != "https://site.com/page" {
		t.Errorf("Canonical = %q", r.Canonical)
	}
	if !slices.Equal(r.H1s, []string{"Main heading"}) {
		t.Errorf("H1s = %q", r.H1s)
	}

	wantInternal := []string{"https://site.com/about", "https://www.site.com/contact"}
	if !slices.Equal(r.InternalLinks, wantInternal) {
		t.Errorf("InternalLinks = %q, want %q", r.InternalLinks, wantInternal)
	}
	wantExternal := []string{"https://other.com/"}
	if !slices.Equal(r.ExternalLinks, wantExternal) {
		t.Errorf("ExternalLinks = %q, want %q", r.ExternalLinks, wantExternal)
	}

	// alt="" is valid for decorative images, so only c.png counts.
	if r.ImagesMissingAlt != 1 {
		t.Errorf("ImagesMissingAlt = %d, want 1", r.ImagesMissingAlt)
	}
}

// TestCrawl runs the real crawler against a tiny fake website.
// httptest.NewServer starts a web server on this computer for the length of the test,
// so the test needs no internet and gives the same result every time.
func TestCrawl(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r) // "/" also catches every unknown path
			return
		}
		fmt.Fprint(w, `<html><head><title>Home</title></head><body><h1>Home</h1>
			<a href="/about">About</a> <a href="/old">Old</a>
			<a href="/missing">Missing</a> <a href="https://example.com/">External</a>
		</body></html>`)
	})
	mux.HandleFunc("/about", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><title>About</title></head><body><h1>About</h1><a href="/">Home</a></body></html>`)
	})
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/about", http.StatusMovedPermanently)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	start, _ := url.Parse(srv.URL)
	pages, inlinks := crawl(context.Background(), newClient(), start, crawlConfig{
		MaxDepth: 2, MaxPages: 10, Workers: 3, Delay: time.Millisecond,
	})

	// Index the reports by URL so the checks below are easy to read.
	byURL := map[string]*PageReport{}
	for _, p := range pages {
		byURL[p.URL] = p
	}

	if len(pages) != 4 {
		t.Fatalf("crawled %d pages, want 4 (home, about, old, missing): %v", len(pages), keys(byURL))
	}
	if byURL[srv.URL+"/"].Title != "Home" {
		t.Errorf("home title = %q", byURL[srv.URL+"/"].Title)
	}
	if got := byURL[srv.URL+"/old"]; got.StatusCode != 301 || got.RedirectTo != srv.URL+"/about" {
		t.Errorf("/old = %d -> %q, want 301 -> %s/about", got.StatusCode, got.RedirectTo, srv.URL)
	}
	if got := byURL[srv.URL+"/missing"].StatusCode; got != 404 {
		t.Errorf("/missing status = %d, want 404", got)
	}
	if got := inlinks[srv.URL+"/missing"]; !slices.Equal(got, []string{srv.URL + "/"}) {
		t.Errorf("/missing linked from %q, want only the home page", got)
	}

	// The whole pipeline: crawl, then checks.
	found := map[string]bool{}
	for _, issue := range runChecks(pages, inlinks) {
		found[issue.Check] = true
	}
	for _, want := range []string{"broken-link", "link-to-redirect"} {
		if !found[want] {
			t.Errorf("expected a %s issue, got %v", want, keys(found))
		}
	}
}

// TestCrawlStopsWhenCancelled checks what happens on Ctrl+C:
// the crawl stops early, and cut-off requests aren't reported as broken links.
func TestCrawlStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The server's handler runs in its own goroutines, so the counter must be
	// safe for concurrent use. atomic.Int32 is, without needing a mutex.
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 3 {
			cancel() // act like the user pressed Ctrl+C during the 3rd request
		}
		// Every page links to 20 more, so the crawl would run a long time on its own.
		fmt.Fprint(w, "<html><head><title>Page</title></head><body>")
		for i := 0; i < 20; i++ {
			fmt.Fprintf(w, `<a href="%s-%d">link</a>`, r.URL.Path, i)
		}
		fmt.Fprint(w, "</body></html>")
	}))
	defer srv.Close()

	start, _ := url.Parse(srv.URL)
	pages, _ := crawl(ctx, newClient(), start, crawlConfig{
		MaxDepth: 5, MaxPages: 500, Workers: 3, Delay: time.Millisecond,
	})

	if len(pages) == 0 || len(pages) > 10 {
		t.Errorf("crawled %d pages, want a few: the crawl should stop soon after cancel", len(pages))
	}
	for _, p := range pages {
		if p.Error != "" {
			t.Errorf("cancelled request reported as an error: %s: %s", p.URL, p.Error)
		}
	}
}

// TestCrawlRespectsRobots checks that disallowed URLs are never fetched.
func TestCrawlRespectsRobots(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: *\nDisallow: /private\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/private") {
			t.Errorf("crawler fetched %s, which robots.txt disallows", r.URL.Path)
		}
		fmt.Fprint(w, `<html><body><a href="/public">Public</a> <a href="/private/page">Private</a></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	start, _ := url.Parse(srv.URL)
	rules, err := fetchRobots(context.Background(), srv.Client(), start)
	if err != nil {
		t.Fatal(err)
	}

	pages, _ := crawl(context.Background(), newClient(), start, crawlConfig{
		MaxDepth: 2, MaxPages: 10, Workers: 2, Delay: time.Millisecond, Robots: rules,
	})
	if len(pages) != 2 {
		t.Errorf("crawled %d pages, want 2 (/ and /public)", len(pages))
	}
}

// TestCrawlUsesSitemap checks orphan pages and bad sitemap URLs.
func TestCrawlUsesSitemap(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/linked":
			fmt.Fprint(w, `<html><head><title>Page</title></head><body><h1>Page</h1><a href="/linked">Linked</a></body></html>`)
		case "/orphan":
			fmt.Fprint(w, `<html><head><title>Orphan</title></head><body><h1>Orphan</h1></body></html>`)
		case "/old":
			http.Redirect(w, r, "/linked", http.StatusMovedPermanently)
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	start, _ := url.Parse(srv.URL)
	pages, inlinks := crawl(context.Background(), newClient(), start, crawlConfig{
		MaxDepth: 2, MaxPages: 20, Workers: 2, Delay: time.Millisecond,
		SitemapURLs: []string{
			srv.URL + "/", srv.URL + "/linked", srv.URL + "/orphan",
			srv.URL + "/old", srv.URL + "/gone", "https://other.com/page",
		},
	})

	byURL := map[string]*PageReport{}
	for _, p := range pages {
		byURL[p.URL] = p
	}
	if p := byURL[srv.URL+"/linked"]; p == nil || p.Depth != 1 || !p.InSitemap {
		t.Errorf("/linked should be found by a link at depth 1 and marked InSitemap, got %+v", p)
	}
	if p := byURL[srv.URL+"/orphan"]; p == nil || p.Depth != depthSitemapOnly {
		t.Errorf("/orphan should be crawled as sitemap-only, got %+v", p)
	}
	if _, ok := byURL["https://other.com/page"]; ok {
		t.Error("crawled a sitemap URL from another site")
	}

	// Which sitemap-related issues were reported, and for which URL?
	got := map[string][]string{}
	for _, issue := range runChecks(pages, inlinks) {
		switch issue.Check {
		case "orphan-page", "sitemap-bad-url", "broken-link":
			got[issue.Subject] = append(got[issue.Subject], issue.Check)
		}
	}
	want := map[string][]string{
		srv.URL + "/orphan": {"orphan-page"},
		srv.URL + "/old":    {"sitemap-bad-url"},
		srv.URL + "/gone":   {"sitemap-bad-url"}, // not also a broken-link: nothing links to it
	}
	if len(got) != len(want) {
		t.Errorf("got issues for %v, want %v", keys(got), keys(want))
	}
	for subject, checks := range want {
		if !slices.Equal(got[subject], checks) {
			t.Errorf("%s: got %v, want %v", subject, got[subject], checks)
		}
	}
}

// keys returns a map's keys, for readable failure messages.
// [V any] makes this a generic function: it works for maps with any value type.
func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}