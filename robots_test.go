package main

import (
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestRobotsRules(t *testing.T) {
	const file = `
# A comment line
User-agent: *
Disallow: /admin/
Allow: /admin/help
Disallow: /*?sort=
Disallow: /*.pdf$

User-agent: Googlebot
Disallow: /

Sitemap: https://site.com/sitemap.xml
`
	r := parseRobots(strings.NewReader(file), robotsAgent)

	tests := []struct {
		path string
		want bool
	}{
		{"/", true}, // Googlebot's "Disallow: /" doesn't apply to us
		{"/blog/post", true},
		{"/admin/", false},
		{"/admin/users", false},
		{"/admin/help", true},       // the longer Allow rule wins
		{"/shop?sort=price", false}, // * matches any characters
		{"/shop?page=2", true},
		{"/guide.pdf", false},    // $ means the URL must end there
		{"/guide.pdf?v=2", true}, // ...so this one is allowed
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			u, _ := url.Parse("https://site.com" + tt.path)
			if got := r.allowed(u); got != tt.want {
				t.Errorf("allowed(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}

	if !slices.Equal(r.sitemaps, []string{"https://site.com/sitemap.xml"}) {
		t.Errorf("sitemaps = %q", r.sitemaps)
	}
}

func TestRobotsGroups(t *testing.T) {
	tests := []struct {
		name string
		file string
		path string
		want bool
	}{
		{"no rules", "", "/anything", true},
		{"block everything", "User-agent: *\nDisallow: /", "/anything", false},
		{"our own group wins over *", "User-agent: *\nDisallow: /\n\nUser-agent: seo-auditor\nDisallow: /private", "/public", true},
		{"our own group still applies", "User-agent: *\nDisallow: /\n\nUser-agent: seo-auditor\nDisallow: /private", "/private", false},
		{"empty Disallow allows all", "User-agent: seo-auditor\nDisallow:\n\nUser-agent: *\nDisallow: /", "/x", true},
		{"names are case-insensitive", "user-agent: SEO-Auditor\ndisallow: /x", "/x", false},
		{"shared group", "User-agent: googlebot\nUser-agent: *\nDisallow: /x", "/x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, _ := url.Parse("https://site.com" + tt.path)
			if got := parseRobots(strings.NewReader(tt.file), robotsAgent).allowed(u); got != tt.want {
				t.Errorf("allowed(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}

	// A nil *robots (no robots.txt at all) allows everything.
	var none *robots
	if u, _ := url.Parse("https://site.com/x"); !none.allowed(u) {
		t.Error("nil robots should allow everything")
	}
}
