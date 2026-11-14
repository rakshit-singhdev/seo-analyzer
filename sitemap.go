package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// maxSitemapFiles stops a site with thousands of sitemap files (or index files
// that point at each other in a loop) from keeping the crawler busy forever.
const maxSitemapFiles = 50

// sitemapFile matches both kinds of sitemap. The struct tags tell encoding/xml
// where each value is: "url>loc" means the <loc> inside each <url>.
type sitemapFile struct {
	Pages    []string `xml:"url>loc"`     // in a <urlset>: page URLs
	Sitemaps []string `xml:"sitemap>loc"` // in a <sitemapindex>: more sitemap files
}

// fetchSitemapURLs returns every page URL listed in the site's sitemaps.
// It starts from the Sitemap: lines in robots.txt, or /sitemap.xml if there are none,
// and follows sitemap index files to the sitemaps they list.
// It returns the URLs from every file that worked, plus one error for all files that didn't.
func fetchSitemapURLs(ctx context.Context, client *http.Client, site *url.URL, fromRobots []string) ([]string, error) {
	// Clone so that appending to queue can never change the caller's slice.
	queue := slices.Clone(fromRobots)
	if len(queue) == 0 {
		queue = []string{site.ResolveReference(&url.URL{Path: "/sitemap.xml"}).String()}
	}

	seen := map[string]bool{}
	var pages []string
	var errs []error

	for len(queue) > 0 && len(seen) < maxSitemapFiles {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true

		file, err := fetchSitemap(ctx, client, next)
		if err != nil {
			errs = append(errs, err) // keep going: other sitemap files may still work
			continue
		}
		pages = append(pages, file.Pages...)
		queue = append(queue, file.Sitemaps...)
	}

	// errors.Join combines several errors into one, and returns nil if errs is empty.
	return pages, errors.Join(errs...)
}

// fetchSitemap downloads one sitemap file and reads its <loc> entries.
// Compressed .xml.gz sitemaps aren't supported.
func fetchSitemap(ctx context.Context, client *http.Client, rawURL string) (*sitemapFile, error) {
	resp, err := get(ctx, client, rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d", rawURL, resp.StatusCode)
	}

	var file sitemapFile
	// 50 MB is the largest size the sitemap standard allows.
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 50<<20)).Decode(&file); err != nil {
		return nil, fmt.Errorf("reading %s: %w", rawURL, err)
	}

	// <loc> values often have spaces or line breaks around the URL.
	// range gives a copy of each element, so change them through the index.
	for i := range file.Pages {
		file.Pages[i] = strings.TrimSpace(file.Pages[i])
	}
	for i := range file.Sitemaps {
		file.Sitemaps[i] = strings.TrimSpace(file.Sitemaps[i])
	}
	return &file, nil
}