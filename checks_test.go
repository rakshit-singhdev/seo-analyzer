package main

import (
	"maps"
	"strings"
	"testing"
)

// page builds a normal 200 HTML page for tests.
// h1s ...string is a variadic parameter: callers can pass any number of H1s.
func page(url, title, desc string, h1s ...string) *PageReport {
	return &PageReport{
		URL:             url,
		StatusCode:      200,
		ContentType:     "text/html; charset=utf-8",
		Title:           title,
		MetaDescription: desc,
		H1s:             h1s,
	}
}

// countByCheck turns a list of issues into "check name -> how many", which is easy to compare.
func countByCheck(issues []Issue) map[string]int {
	counts := map[string]int{}
	for _, issue := range issues {
		counts[issue.Check]++
	}
	return counts
}

func TestCheckTitles(t *testing.T) {
	pdf := page("https://site.com/file.pdf", "", "")
	pdf.ContentType = "application/pdf"

	tests := []struct {
		name  string
		pages []*PageReport
		want  map[string]int
	}{
		{"good title", []*PageReport{page("https://site.com/", "Home | Site", "")}, map[string]int{}},
		{"missing title", []*PageReport{page("https://site.com/", "", "")}, map[string]int{"missing-title": 1}},
		{"61 characters is long", []*PageReport{page("https://site.com/", strings.Repeat("x", 61), "")}, map[string]int{"long-title": 1}},
		{"60 characters is fine", []*PageReport{page("https://site.com/", strings.Repeat("x", 60), "")}, map[string]int{}},
		// é takes 2 bytes, so this title is 120 bytes but only 60 characters.
		{"counts characters, not bytes", []*PageReport{page("https://site.com/", strings.Repeat("é", 60), "")}, map[string]int{}},
		{"duplicate titles", []*PageReport{
			page("https://site.com/a", "Same", ""),
			page("https://site.com/b", "Same", ""),
		}, map[string]int{"duplicate-title": 1}},
		{"non-HTML pages are skipped", []*PageReport{pdf}, map[string]int{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Passing nil for inlinks is fine: reading from a nil map returns zero values.
			got := countByCheck(checkTitles(tt.pages, nil))
			if !maps.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckH1s(t *testing.T) {
	tests := []struct {
		name string
		h1s  []string
		want map[string]int
	}{
		{"one h1", []string{"Hello"}, map[string]int{}},
		{"no h1", nil, map[string]int{"missing-h1": 1}},
		{"two h1s", []string{"A", "B"}, map[string]int{"multiple-h1": 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// tt.h1s... spreads the slice into the variadic parameter.
			p := page("https://site.com/", "Title", "Desc", tt.h1s...)
			got := countByCheck(checkH1s([]*PageReport{p}, nil))
			if !maps.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLinkChecks(t *testing.T) {
	// In a []*PageReport literal, {...} is short for &PageReport{...}.
	pages := []*PageReport{
		page("https://site.com/", "Home", "Desc", "Home"),
		{URL: "https://site.com/old", StatusCode: 301, RedirectTo: "https://site.com/new"},
		{URL: "https://site.com/gone", StatusCode: 404},
		{URL: "https://site.com/down", Error: "timeout"},
		// The start URL redirecting is normal; no page links to it.
		{URL: "http://site.com/", StatusCode: 301, RedirectTo: "https://site.com/"},
	}
	inlinks := map[string][]string{
		"https://site.com/old":  {"https://site.com/"},
		"https://site.com/gone": {"https://site.com/", "https://site.com/about"},
		"https://site.com/down": {"https://site.com/"},
	}

	broken := checkBrokenLinks(pages, inlinks)
	if len(broken) != 2 {
		t.Fatalf("got %d broken-link issues, want 2 (gone, down): %+v", len(broken), broken)
	}
	for _, issue := range broken {
		if issue.Subject == "https://site.com/gone" && len(issue.Pages) != 2 {
			t.Errorf("/gone should list 2 linking pages, got %q", issue.Pages)
		}
	}

	redirects := checkLinksToRedirects(pages, inlinks)
	if len(redirects) != 1 || redirects[0].Subject != "https://site.com/old" {
		t.Errorf("want one link-to-redirect issue for /old, got %+v", redirects)
	}
}
