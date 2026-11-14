package main

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// Severity says how serious an issue is.
type Severity int

// Inside a const block, iota counts up from 0: SeverityError = 0, SeverityWarning = 1.
// Lower numbers sort first, so errors are listed before warnings.
const (
	SeverityError Severity = iota
	SeverityWarning
)

// String makes Severity satisfy the fmt.Stringer interface, so printing it
// with %s shows "error" instead of 0. Go interfaces are satisfied implicitly:
// there is no "implements" keyword; having the right method is enough.
func (s Severity) String() string {
	if s == SeverityError {
		return "error"
	}
	return "warning"
}

// Issue is one problem found by a check.
type Issue struct {
	Severity Severity
	Check    string // short name, e.g. "duplicate-title"
	Subject  string // the URL or text the issue is about
	Detail   string
	Pages    []string // pages involved: where a link appears, or which pages share a text
}

// check is a function type. Every check has this same signature,
// so they can all live in one slice and run in one loop.
type check func(pages []*PageReport, inlinks map[string][]string) []Issue

var checks = []check{
	checkBrokenLinks,
	checkLinksToRedirects,
	checkTitles,
	checkDescriptions,
	checkH1s,
	checkSitemap,
}

// Rough guides only: Google shortens titles and descriptions by pixel width, not characters.
const (
	maxTitleLen       = 60
	maxDescriptionLen = 160
)

func runChecks(pages []*PageReport, inlinks map[string][]string) []Issue {
	var issues []Issue
	for _, c := range checks {
		// ... spreads the returned slice into separate arguments for append.
		issues = append(issues, c(pages, inlinks)...)
	}

	// Sort once here, so the terminal output and the Excel file share the same order.
	// cmp.Or returns the first comparison that isn't 0:
	// sort by severity, then by check name, then by subject.
	slices.SortFunc(issues, func(a, b Issue) int {
		return cmp.Or(
			cmp.Compare(a.Severity, b.Severity),
			strings.Compare(a.Check, b.Check),
			strings.Compare(a.Subject, b.Subject),
		)
	})
	return issues
}

// isOKPage reports whether a page loaded as normal HTML, so on-page checks apply to it.
func isOKPage(r *PageReport) bool {
	return r.Error == "" && r.StatusCode == 200 && isHTML(r.ContentType)
}

func checkBrokenLinks(pages []*PageReport, inlinks map[string][]string) []Issue {
	var issues []Issue
	for _, r := range pages {
		if r.Depth == depthSitemapOnly {
			continue // no link points here; checkSitemap reports it instead
		}
		var problem string
		switch {
		case r.Error != "":
			problem = r.Error
		case r.StatusCode >= 400:
			problem = fmt.Sprintf("returns %d", r.StatusCode)
		default:
			continue // this page is fine, move on to the next one
		}
		issues = append(issues, Issue{
			Severity: SeverityError,
			Check:    "broken-link",
			Subject:  r.URL,
			Detail:   problem,
			Pages:    inlinks[r.URL],
		})
	}
	return issues
}

func checkLinksToRedirects(pages []*PageReport, inlinks map[string][]string) []Issue {
	var issues []Issue
	for _, r := range pages {
		sources := inlinks[r.URL]
		// A start URL that redirects (http -> https) has no sources and isn't a problem.
		if r.RedirectTo == "" || len(sources) == 0 {
			continue
		}
		issues = append(issues, Issue{
			Severity: SeverityWarning,
			Check:    "link-to-redirect",
			Subject:  r.URL,
			Detail:   fmt.Sprintf("%d -> %s", r.StatusCode, r.RedirectTo),
			Pages:    sources,
		})
	}
	return issues
}

// The "_" parameter name means this check doesn't use inlinks,
// but it still needs the parameter to match the check type.
func checkTitles(pages []*PageReport, _ map[string][]string) []Issue {
	var issues []Issue
	byTitle := map[string][]string{} // title -> pages that use it

	for _, r := range pages {
		if !isOKPage(r) {
			continue
		}
		n := utf8.RuneCountInString(r.Title)
		if n == 0 {
			issues = append(issues, Issue{
				Severity: SeverityError,
				Check:    "missing-title",
				Subject:  r.URL,
				Detail:   "page has no <title>",
			})
			continue
		}
		if n > maxTitleLen {
			issues = append(issues, Issue{
				Severity: SeverityWarning,
				Check:    "long-title",
				Subject:  r.URL,
				Detail:   fmt.Sprintf("%d characters (aim for %d or fewer): %q", n, maxTitleLen, r.Title),
			})
		}
		byTitle[r.Title] = append(byTitle[r.Title], r.URL)
	}

	return append(issues, duplicates(byTitle, "duplicate-title")...)
}

func checkDescriptions(pages []*PageReport, _ map[string][]string) []Issue {
	var issues []Issue
	byDesc := map[string][]string{} // description -> pages that use it

	for _, r := range pages {
		if !isOKPage(r) {
			continue
		}
		n := utf8.RuneCountInString(r.MetaDescription)
		if n == 0 {
			issues = append(issues, Issue{
				Severity: SeverityWarning,
				Check:    "missing-description",
				Subject:  r.URL,
				Detail:   "page has no meta description",
			})
			continue
		}
		if n > maxDescriptionLen {
			issues = append(issues, Issue{
				Severity: SeverityWarning,
				Check:    "long-description",
				Subject:  r.URL,
				Detail:   fmt.Sprintf("%d characters (aim for %d or fewer)", n, maxDescriptionLen),
			})
		}
		byDesc[r.MetaDescription] = append(byDesc[r.MetaDescription], r.URL)
	}

	return append(issues, duplicates(byDesc, "duplicate-description")...)
}

func checkH1s(pages []*PageReport, _ map[string][]string) []Issue {
	var issues []Issue
	for _, r := range pages {
		if !isOKPage(r) {
			continue
		}
		// A switch can start with a short statement, like an if.
		switch n := len(r.H1s); {
		case n == 0:
			issues = append(issues, Issue{
				Severity: SeverityWarning,
				Check:    "missing-h1",
				Subject:  r.URL,
				Detail:   "page has no <h1>",
			})
		case n > 1:
			issues = append(issues, Issue{
				Severity: SeverityWarning,
				Check:    "multiple-h1",
				Subject:  r.URL,
				Detail:   fmt.Sprintf("%d <h1> tags", n),
			})
		}
	}
	return issues
}

// checkSitemap flags sitemap URLs that don't work as listed, and pages that are
// only reachable through the sitemap.
func checkSitemap(pages []*PageReport, _ map[string][]string) []Issue {
	var issues []Issue
	for _, r := range pages {
		if !r.InSitemap {
			continue
		}

		var problem string
		switch {
		case r.Error != "":
			problem = r.Error
		case r.RedirectTo != "":
			problem = fmt.Sprintf("%d -> %s", r.StatusCode, r.RedirectTo)
		case r.StatusCode != 200:
			problem = fmt.Sprintf("returns %d", r.StatusCode)
		}

		switch {
		case problem != "":
			issues = append(issues, Issue{
				Severity: SeverityWarning,
				Check:    "sitemap-bad-url",
				Subject:  r.URL,
				Detail:   problem + " (a sitemap should list only final, working URLs)",
			})
		case r.Depth == depthSitemapOnly:
			issues = append(issues, Issue{
				Severity: SeverityWarning,
				Check:    "orphan-page",
				Subject:  r.URL,
				Detail:   "in the sitemap, but no crawled page links to it (or it is deeper than -depth)",
			})
		}
	}
	return issues
}

// duplicates turns a "text -> pages" map into one issue for each text used on 2+ pages.
func duplicates(groups map[string][]string, checkName string) []Issue {
	var issues []Issue
	for text, urls := range groups {
		if len(urls) < 2 {
			continue
		}
		issues = append(issues, Issue{
			Severity: SeverityWarning,
			Check:    checkName,
			Subject:  fmt.Sprintf("%q", text),
			Detail:   fmt.Sprintf("used on %d pages", len(urls)),
			Pages:    urls,
		})
	}
	return issues
}

// listPaths shows up to 5 URLs as paths ("/plans/") to keep lines short.
func listPaths(urls []string) string {
	if len(urls) == 0 {
		return "(no crawled page)"
	}

	// Clone before sorting: a slice shares its data with the original,
	// so sorting in place would also reorder the caller's slice.
	sorted := slices.Clone(urls)
	slices.Sort(sorted)

	const show = 5
	var parts []string
	for _, u := range sorted[:min(show, len(sorted))] {
		parts = append(parts, shortPath(u))
	}

	s := strings.Join(parts, ", ")
	if len(sorted) > show {
		s += fmt.Sprintf(" and %d more", len(sorted)-show)
	}
	return s
}

func shortPath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.RequestURI() // path plus query, e.g. "/signup?planId=free"
}

func printIssues(issues []Issue) {
	errorCount := 0
	for _, issue := range issues {
		if issue.Severity == SeverityError {
			errorCount++
		}
	}
	fmt.Printf("\nIssues found: %d error(s), %d warning(s)\n", errorCount, len(issues)-errorCount)

	lastCheck := ""
	for _, issue := range issues {
		if issue.Check != lastCheck {
			fmt.Printf("\n[%s] %s\n", issue.Severity, issue.Check)
			lastCheck = issue.Check
		}
		fmt.Printf("  %s\n    %s\n", issue.Subject, issue.Detail)
		if len(issue.Pages) > 0 {
			fmt.Printf("    pages: %s\n", listPaths(issue.Pages))
		}
	}
}