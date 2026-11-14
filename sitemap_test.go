package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestFetchSitemapURLs(t *testing.T) {
	mux := http.NewServeMux()
	// Handlers build absolute URLs from r.Host, the address the request was sent to.
	// %[1]s in the format means "use the 1st argument", so one argument fills every spot.
	mux.HandleFunc("/sitemap_index.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>http://%[1]s/pages.xml</loc></sitemap>
  <sitemap><loc>http://%[1]s/missing.xml</loc></sitemap>
  <sitemap><loc>http://%[1]s/sitemap_index.xml</loc></sitemap>
</sitemapindex>`, r.Host)
	})
	mux.HandleFunc("/pages.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>http://%[1]s/</loc></url>
  <url>
    <loc>
      http://%[1]s/about
    </loc>
  </url>
</urlset>`, r.Host)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset><url><loc>http://%s/default</loc></url></urlset>`, r.Host)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	site, _ := url.Parse(srv.URL)

	t.Run("index file, a missing file and a loop", func(t *testing.T) {
		got, err := fetchSitemapURLs(context.Background(), srv.Client(), site, []string{srv.URL + "/sitemap_index.xml"})

		// The index lists itself; the seen map stops that from looping forever.
		want := []string{srv.URL + "/", srv.URL + "/about"}
		if !slices.Equal(got, want) {
			t.Errorf("got %q, want %q", got, want)
		}
		// missing.xml is reported, but the URLs from the other files still come back.
		if err == nil || !strings.Contains(err.Error(), "missing.xml returned 404") {
			t.Errorf("want an error about missing.xml, got %v", err)
		}
	})

	t.Run("no Sitemap lines in robots.txt", func(t *testing.T) {
		got, err := fetchSitemapURLs(context.Background(), srv.Client(), site, nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{srv.URL + "/default"}; !slices.Equal(got, want) {
			t.Errorf("got %q, want %q (from /sitemap.xml)", got, want)
		}
	})
}