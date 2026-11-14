package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// robotsAgent is the name a robots.txt file would use to give this crawler its own rules.
const robotsAgent = "seo-auditor"

// robots holds the robots.txt rules that apply to this crawler.
type robots struct {
	rules    []robotsRule
	sitemaps []string // the file's Sitemap: lines (used in the next step)
}

type robotsRule struct {
	allow   bool
	pattern *regexp.Regexp
	length  int // length of the rule's path: the longest matching rule wins
}

// get makes a GET request with our User-Agent.
func get(ctx context.Context, client *http.Client, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return client.Do(req)
}

// fetchRobots downloads and parses the site's robots.txt.
// It never returns nil: when there's no usable file, the result allows everything.
func fetchRobots(ctx context.Context, client *http.Client, site *url.URL) (*robots, error) {
	robotsURL := site.ResolveReference(&url.URL{Path: "/robots.txt"})
	resp, err := get(ctx, client, robotsURL.String())
	if err != nil {
		return &robots{}, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 500:
		return &robots{}, fmt.Errorf("%s returned %d", robotsURL, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return &robots{}, nil // no robots.txt (404) means the whole site may be crawled
	}

	// Google reads at most 500 KB of a robots.txt file, so we do too.
	return parseRobots(io.LimitReader(resp.Body, 500<<10), robotsAgent), nil
}

// parseRobots reads a robots.txt file and keeps the rules for agent,
// or the rules for "*" if no group names agent.
func parseRobots(r io.Reader, agent string) *robots {
	result := &robots{}
	groups := map[string][]robotsRule{} // user-agent -> its rules
	var agents []string                 // user-agents of the group being read
	groupHasRules := false

	scanner := bufio.NewScanner(r) // reads the file one line at a time
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i] // drop comments
		}

		// Cut splits at the first ":", so "Sitemap: https://..." keeps its URL whole.
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			// User-agent lines in a row share one group; one that comes
			// after rules starts a new group.
			if groupHasRules {
				agents = nil
				groupHasRules = false
			}
			name := strings.ToLower(value)
			agents = append(agents, name)
			if _, ok := groups[name]; !ok {
				groups[name] = nil // the group exists even if it ends up with no rules
			}
		case "allow", "disallow":
			groupHasRules = true
			if value == "" {
				continue // "Disallow:" with nothing after it allows everything
			}
			rule := robotsRule{allow: key == "allow", pattern: robotsPattern(value), length: len(value)}
			for _, name := range agents {
				groups[name] = append(groups[name], rule)
			}
		case "sitemap":
			result.sitemaps = append(result.sitemaps, value)
		}
	}

	// A group for our own name wins over "*", the same way Googlebot's group does for Google.
	if rules, ok := groups[agent]; ok {
		result.rules = rules
	} else {
		result.rules = groups["*"]
	}
	return result
}

// robotsPattern turns a robots.txt path into a regular expression.
// In robots.txt, * matches any characters, and a $ at the end means "the URL ends here".
func robotsPattern(path string) *regexp.Regexp {
	anchored := strings.HasSuffix(path, "$")
	path = strings.TrimSuffix(path, "$")

	// QuoteMeta escapes characters like ? and . so they match literally;
	// then the escaped \* is turned back into "any characters".
	expr := "^" + strings.ReplaceAll(regexp.QuoteMeta(path), `\*`, ".*")
	if anchored {
		expr += "$"
	}
	// MustCompile panics on an invalid expression, but QuoteMeta guarantees this one is valid.
	return regexp.MustCompile(expr)
}

// allowed reports whether robots.txt lets us fetch u.
// As with Google, the longest matching rule wins, and Allow wins a tie.
// Methods can be called on a nil pointer: a nil *robots means "no rules".
func (r *robots) allowed(u *url.URL) bool {
	if r == nil {
		return true
	}

	target := u.EscapedPath()
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}

	best, allow := -1, true
	for _, rule := range r.rules {
		if !rule.pattern.MatchString(target) {
			continue
		}
		if rule.length > best || (rule.length == best && rule.allow) {
			best, allow = rule.length, rule.allow
		}
	}
	return allow
}
