// seo-auditor — step 5: crawl a site, report SEO issues, save an Excel report.
//
// Usage:
//
//	go run . -url https://example.com -max 200 -out audit.xlsx
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// version is set when building, with: go build -ldflags "-X main.version=1.0.0"
// It has to be a var, not a const, because -X can only change variables.
var version = "dev"

// userAgent identifies this crawler to the sites it visits.
const userAgent = "seo-auditor/0.1 (learning project)"

// PageReport holds everything we learn about a single page.
type PageReport struct {
	URL              string
	Depth            int    // how many clicks from the start page (depthSitemapOnly if no link led here)
	InSitemap        bool   // listed in the site's sitemap
	Error            string // network failure, timeout, etc.
	StatusCode       int
	ContentType      string
	ResponseTime     time.Duration
	RedirectTo       string // absolute URL, set when the server answers with a 3xx
	Title            string
	MetaDescription  string
	Canonical        string
	H1s              []string
	InternalLinks    []string
	ExternalLinks    []string
	ImagesMissingAlt int
}

// queueItem is one URL waiting to be crawled.
type queueItem struct {
	url   string
	depth int
}

// depthSitemapOnly is the depth of pages found only through the sitemap:
// no crawled page links to them.
const depthSitemapOnly = -1

// crawlConfig holds the settings for one crawl.
// A nil Robots means there are no robots.txt rules to follow.
type crawlConfig struct {
	MaxDepth    int
	MaxPages    int
	Workers     int
	Delay       time.Duration
	Robots      *robots
	SitemapURLs []string // page URLs from the sitemap, crawled after the links if no link led to them
}

func main() {
	target := flag.String("url", "", "site to audit, e.g. https://example.com")
	maxDepth := flag.Int("depth", 2, "how many clicks deep to follow links from the start page")
	maxPages := flag.Int("max", 50, "stop after this many pages")
	workers := flag.Int("workers", 5, "how many pages to fetch at the same time")
	delay := flag.Duration("delay", 200*time.Millisecond, "minimum time between requests, shared by all workers")
	out := flag.String("out", "", "also save the report to this Excel file, e.g. audit.xlsx")
	ignoreRobots := flag.Bool("ignore-robots", false, "crawl pages that robots.txt disallows (only for sites you manage)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("seo-auditor", version)
		return
	}

	if *target == "" {
		fmt.Fprintln(os.Stderr, "usage: seo-auditor -url https://example.com [-depth 2] [-max 50] [-workers 5] [-delay 200ms] [-out audit.xlsx] [-ignore-robots]")
		os.Exit(1)
	}
	if *workers < 1 || *delay <= 0 {
		fmt.Fprintln(os.Stderr, "error: -workers must be at least 1 and -delay must be greater than 0")
		os.Exit(1)
	}
	// Check this before crawling, so a typo doesn't waste a long crawl.
	if *out != "" && !strings.HasSuffix(strings.ToLower(*out), ".xlsx") {
		fmt.Fprintln(os.Stderr, "error: -out must end in .xlsx, e.g. -out audit.xlsx")
		os.Exit(1)
	}

	startURL, err := url.Parse(*target)
	if err != nil || (startURL.Scheme != "http" && startURL.Scheme != "https") {
		fmt.Fprintf(os.Stderr, "error: %q is not a valid http(s) URL\n", *target)
		os.Exit(1)
	}

	// ctx is cancelled when the user presses Ctrl+C (os.Interrupt).
	// Everything that receives ctx can then stop what it is doing.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore normal Ctrl+C, so pressing it a second time quits immediately
	}()

	// robots.txt may redirect (http -> https, or to www), so it's fetched with
	// a client that follows redirects, unlike the crawl client.
	seedClient := &http.Client{Timeout: 15 * time.Second}
	rules, err := fetchRobots(ctx, seedClient, startURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not read robots.txt, crawling without it:", err)
	}
	if *ignoreRobots {
		rules.rules = nil
		fmt.Fprintln(os.Stderr, "robots.txt: ignored because of -ignore-robots")
	} else {
		fmt.Fprintf(os.Stderr, "robots.txt: %d rule(s) apply to this crawler\n", len(rules.rules))
	}

	sitemapURLs, err := fetchSitemapURLs(ctx, seedClient, startURL, rules.sitemaps)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: problem reading the sitemap:", err)
	}
	fmt.Fprintf(os.Stderr, "sitemap: %d URL(s) listed\n", len(sitemapURLs))

	client := newClient()
	reports, inlinks := crawl(ctx, client, startURL, crawlConfig{
		MaxDepth:    *maxDepth,
		MaxPages:    *maxPages,
		Workers:     *workers,
		Delay:       *delay,
		Robots:      rules,
		SitemapURLs: sitemapURLs,
	})
	issues := runChecks(reports, inlinks)
	printSummary(reports)
	printIssues(issues)

	if *out != "" {
		if err := writeExcel(*out, reports, issues); err != nil {
			fmt.Fprintln(os.Stderr, "error saving report:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "Report saved to", *out)
	}
}

func newClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// crawl fetches pages with a pool of worker goroutines.
// Only this function touches queue, visited and inlinks, so no mutex is needed:
// workers receive URLs on the jobs channel and send reports back on results.
// It returns the page reports and, for every internal URL, the pages that link to it.
// When ctx is cancelled, it stops handing out URLs and returns what it has so far.
func crawl(ctx context.Context, client *http.Client, start *url.URL, cfg crawlConfig) ([]*PageReport, map[string][]string) {
	startKey := normalizeURL(start)

	queue := []queueItem{{url: startKey, depth: 0}}
	visited := map[string]bool{startKey: true}
	var reports []*PageReport
	inlinks := map[string][]string{} // target URL -> pages that link to it
	blocked := 0                     // URLs skipped because robots.txt disallows them

	// The sitemap's URLs for this site, normalized so they match crawled URLs.
	// The map answers "is this URL in the sitemap?"; the slice keeps the sitemap's order.
	inSitemap := map[string]bool{}
	var sitemapPages []string
	for _, raw := range cfg.SitemapURLs {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !sameSite(u.Host, start.Host) {
			continue
		}
		key := normalizeURL(u)
		if !inSitemap[key] {
			inSitemap[key] = true
			sitemapPages = append(sitemapPages, key)
		}
	}
	sitemapAdded := false

	enqueue := func(link string, depth int) {
		if visited[link] {
			return
		}
		visited[link] = true // mark it even if it's blocked, so it's only counted once
		if u, err := url.Parse(link); err == nil && !cfg.Robots.allowed(u) {
			blocked++
			return
		}
		queue = append(queue, queueItem{url: link, depth: depth})
	}

	jobs := make(chan queueItem)
	results := make(chan *PageReport)

	// One shared ticker means one request per tick across ALL workers,
	// so adding workers never makes the crawl hit the server harder than -delay allows.
	ticker := time.NewTicker(cfg.Delay)
	defer ticker.Stop()

	for i := 0; i < cfg.Workers; i++ {
		go worker(ctx, client, jobs, results, ticker.C)
	}

	dispatched := 0 // URLs handed to a worker so far
	inFlight := 0   // URLs a worker is fetching right now

	// canDispatch reports whether there's a URL to hand out and we're allowed to.
	// ctx.Err() becomes non-nil once the context is cancelled.
	canDispatch := func() bool {
		return len(queue) > 0 && dispatched < cfg.MaxPages && ctx.Err() == nil
	}

	// Keep looping while workers are busy, even after Ctrl+C,
	// so every goroutine finishes before crawl returns.
	for {
		if inFlight == 0 && !canDispatch() {
			// The link crawl is finished. Queue the sitemap pages no link led to,
			// once, and go round again; the second time, stop.
			if sitemapAdded {
				break
			}
			sitemapAdded = true
			for _, page := range sitemapPages {
				enqueue(page, depthSitemapOnly)
			}
			continue
		}

		// Sending on a nil channel is never ready, so leaving sendTo nil
		// switches off the send case below when there's nothing to send.
		var sendTo chan queueItem
		var next queueItem
		if canDispatch() {
			sendTo = jobs
			next = queue[0]
		}

		// select waits until one of its cases can proceed.
		select {
		case sendTo <- next:
			queue = queue[1:]
			dispatched++
			inFlight++

		case report := <-results:
			inFlight--
			// After Ctrl+C, requests that were cut off come back as errors.
			// They aren't broken links, so leave them out of the report.
			// (continue here moves on to the next round of the for loop.)
			if ctx.Err() != nil && report.Error != "" {
				continue
			}
			report.InSitemap = inSitemap[report.URL]
			reports = append(reports, report)
			fmt.Fprintf(os.Stderr, "[%d] depth %d  %s\n", len(reports), report.Depth, report.URL)

			// Record every internal link, even on pages too deep to follow,
			// so checks can say where a broken or redirecting link was found.
			for _, link := range report.InternalLinks {
				inlinks[link] = append(inlinks[link], report.URL)
			}

			// A redirect isn't a click, so its target keeps the same depth.
			if report.RedirectTo != "" && isInternal(report.RedirectTo, start.Host) {
				enqueue(report.RedirectTo, report.Depth)
			}
			// Links on sitemap-only pages aren't followed: they aren't part of the site's link structure.
			if report.Depth >= 0 && report.Depth < cfg.MaxDepth {
				for _, link := range report.InternalLinks {
					enqueue(link, report.Depth+1)
				}
			}
		}
	}
	close(jobs) // no more work: each worker's range loop ends and the goroutine exits

	switch {
	case ctx.Err() != nil:
		fmt.Fprintf(os.Stderr, "Crawl stopped early: the report covers the %d pages crawled so far\n", len(reports))
	case len(queue) > 0:
		fmt.Fprintf(os.Stderr, "Reached -max %d: %d more URLs were found but not crawled\n", cfg.MaxPages, len(queue))
	}
	if blocked > 0 {
		fmt.Fprintf(os.Stderr, "Skipped %d URL(s) that robots.txt doesn't allow\n", blocked)
	}

	// Reports arrive in whatever order fetches finish, so sort them for a stable report.
	slices.SortFunc(reports, func(a, b *PageReport) int {
		if a.Depth != b.Depth {
			return cmp.Compare(a.Depth, b.Depth)
		}
		return strings.Compare(a.URL, b.URL)
	})

	return reports, inlinks
}

// worker fetches URLs from jobs until the channel is closed.
// <-chan is receive-only and chan<- is send-only, so the compiler
// stops a worker from sending on jobs or reading from results by mistake.
func worker(ctx context.Context, client *http.Client, jobs <-chan queueItem, results chan<- *PageReport, tick <-chan time.Time) {
	for item := range jobs {
		// Wait for the shared rate limiter, but give up at once if the crawl is stopping.
		select {
		case <-tick:
		case <-ctx.Done():
			// Still send a result, so crawl's inFlight count stays correct.
			results <- &PageReport{URL: item.url, Error: ctx.Err().Error()}
			continue
		}

		report, err := fetchPage(ctx, client, item.url)
		if err != nil {
			report = &PageReport{URL: item.url, Error: err.Error()}
		}
		report.Depth = item.depth
		results <- report
	}
}

// isInternal reports whether rawURL belongs to the site being crawled.
func isInternal(rawURL, siteHost string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && sameSite(u.Host, siteHost)
}

func fetchPage(ctx context.Context, client *http.Client, rawURL string) (*PageReport, error) {
	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("url must start with http:// or https://, got %q", rawURL)
	}

	// A request made with ctx is cancelled the moment ctx is, even mid-download.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	report := &PageReport{
		URL:         rawURL,
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
	}

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		report.ResponseTime = time.Since(start)
		// Location can be relative ("/new-page"), so resolve it against this page.
		if loc, err := base.Parse(resp.Header.Get("Location")); err == nil {
			report.RedirectTo = normalizeURL(loc)
		}
		return report, nil
	}

	if !isHTML(report.ContentType) {
		report.ResponseTime = time.Since(start)
		return report, nil
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	report.ResponseTime = time.Since(start)
	if err != nil {
		return nil, fmt.Errorf("parsing HTML: %w", err)
	}

	parsePage(doc, base, report)
	return report, nil
}

func parsePage(doc *goquery.Document, base *url.URL, r *PageReport) {
	r.Title = strings.TrimSpace(doc.Find("head title").First().Text())

	desc, _ := doc.Find(`meta[name="description"]`).Attr("content")
	r.MetaDescription = strings.TrimSpace(desc)
	r.Canonical, _ = doc.Find(`link[rel="canonical"]`).Attr("href")

	doc.Find("h1").Each(func(_ int, s *goquery.Selection) {
		r.H1s = append(r.H1s, strings.TrimSpace(s.Text()))
	})

	seen := map[string]bool{}
	doc.Find("a[href]").Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		link, err := base.Parse(href)
		if err != nil || (link.Scheme != "http" && link.Scheme != "https") {
			return
		}
		// Cloudflare replaces email links with /cdn-cgi/l/email-protection and turns
		// them back into email links with JavaScript. A crawler just sees a 404.
		if strings.HasPrefix(link.Path, "/cdn-cgi/") {
			return
		}
		key := normalizeURL(link)
		if seen[key] {
			return
		}
		seen[key] = true

		if sameSite(link.Host, base.Host) {
			r.InternalLinks = append(r.InternalLinks, key)
		} else {
			r.ExternalLinks = append(r.ExternalLinks, key)
		}
	})

	doc.Find("img").Each(func(_ int, s *goquery.Selection) {
		if _, exists := s.Attr("alt"); !exists {
			r.ImagesMissingAlt++
		}
	})
}

// normalizeURL turns different spellings of the same URL into one string,
// so the visited map doesn't treat them as separate pages.
func normalizeURL(u *url.URL) string {
	n := *u // copy the struct so the caller's URL isn't modified

	n.Scheme = strings.ToLower(n.Scheme)
	n.Host = strings.ToLower(n.Host)
	n.Fragment = "" // "/page#top" is the same page as "/page"

	// "https://site.com:443/" is the same as "https://site.com/"
	if (n.Scheme == "http" && n.Port() == "80") || (n.Scheme == "https" && n.Port() == "443") {
		n.Host = n.Hostname()
	}

	if n.Path == "" {
		n.Path = "/" // "https://site.com" -> "https://site.com/"
	}

	// Drop tracking parameters; they don't change the page content.
	q := n.Query()
	for key := range q {
		k := strings.ToLower(key)
		if strings.HasPrefix(k, "utm_") || k == "fbclid" || k == "gclid" {
			q.Del(key) // deleting from a map while ranging over it is safe in Go
		}
	}
	n.RawQuery = q.Encode() // Encode sorts keys, so ?b=2&a=1 equals ?a=1&b=2
	n.ForceQuery = false    // "page?" -> "page"

	return n.String()
}

// sameSite treats "www.site.com" and "site.com" as the same site.
func sameSite(hostA, hostB string) bool {
	a := strings.TrimPrefix(strings.ToLower(hostA), "www.")
	b := strings.TrimPrefix(strings.ToLower(hostB), "www.")
	return a == b
}

func isHTML(contentType string) bool {
	return strings.Contains(contentType, "text/html")
}

func printSummary(reports []*PageReport) {
	// tabwriter lines up columns separated by \t.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tTIME\tDEPTH\tURL\tNOTE")

	statusCounts := map[int]int{}
	errorCount := 0

	for _, r := range reports {
		status := fmt.Sprint(r.StatusCode)
		var note string

		switch {
		case r.Error != "":
			status = "ERR"
			note = r.Error
			errorCount++
		case r.RedirectTo != "":
			note = "-> " + r.RedirectTo
		case !isHTML(r.ContentType):
			note = r.ContentType
		default:
			note = truncate(r.Title, 50)
		}
		if r.Error == "" {
			statusCounts[r.StatusCode]++
		}

		depth := fmt.Sprint(r.Depth)
		if r.Depth == depthSitemapOnly {
			depth = "sitemap" // found only through the sitemap
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			status, r.ResponseTime.Round(time.Millisecond), depth, r.URL, note)
	}
	w.Flush()

	// Maps have no fixed order, so collect the keys and sort them.
	codes := make([]int, 0, len(statusCounts))
	for code := range statusCounts {
		codes = append(codes, code)
	}
	slices.Sort(codes)

	fmt.Printf("\nCrawled %d pages\n", len(reports))
	for _, code := range codes {
		fmt.Printf("  %d: %d\n", code, statusCounts[code])
	}
	if errorCount > 0 {
		fmt.Printf("  errors: %d\n", errorCount)
	}
}

func truncate(s string, max int) string {
	runes := []rune(s) // slice by characters, not bytes
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}