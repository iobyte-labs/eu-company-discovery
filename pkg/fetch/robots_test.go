package fetch

import (
	"testing"
	"time"
)

const ourAgent = "eucd/0.1.0 (+https://github.com/iobyte-labs/eu-company-discovery)"

func TestParseRobotsPicksTheGroupThatNamesUs(t *testing.T) {
	body := []byte(`
# A directory that welcomes search engines and asks us to stay out of one area.
User-agent: *
Disallow: /

User-agent: Googlebot
Allow: /

User-agent: eucd
Disallow: /private
Crawl-delay: 2
`)
	rules := parseRobots(body, ourAgent)

	if rules.crawlDelay != 2*time.Second {
		t.Errorf("crawlDelay = %s, want 2s", rules.crawlDelay)
	}
	// Our own group applies, so the blanket "Disallow: /" for everyone else does not.
	if allowed, _ := rules.allows("/members"); !allowed {
		t.Error("/members was disallowed; the group naming us allows everything but /private")
	}
	if allowed, pattern := rules.allows("/private/list"); allowed {
		t.Error("/private/list was allowed; our group disallows /private")
	} else if pattern != "/private" {
		t.Errorf("refusing rule = %q, want /private", pattern)
	}
}

func TestParseRobotsFallsBackToTheWildcardGroup(t *testing.T) {
	body := []byte("User-agent: Googlebot\nDisallow:\n\nUser-agent: *\nDisallow: /members\n")
	rules := parseRobots(body, ourAgent)

	if allowed, _ := rules.allows("/members"); allowed {
		t.Error("/members was allowed; the wildcard group disallows it and nothing names us")
	}
	if allowed, _ := rules.allows("/about"); !allowed {
		t.Error("/about was disallowed; only /members is")
	}
}

func TestParseRobotsEmptyAndAbsentMeanAllowed(t *testing.T) {
	// "Disallow:" with no value is the documented way to allow everything. Reading it as a
	// rule matching every path would refuse the whole site.
	for name, body := range map[string]string{
		"empty disallow": "User-agent: *\nDisallow:\n",
		"no rules":       "User-agent: *\n",
		"empty file":     "",
		"only a sitemap": "Sitemap: https://acme.example.org/sitemap.xml\n",
		"only comments":  "# nothing to see\n",
	} {
		rules := parseRobots([]byte(body), ourAgent)
		if allowed, pattern := rules.allows("/members"); !allowed {
			t.Errorf("%s: /members refused by %q, want allowed", name, pattern)
		}
	}
}

func TestParseRobotsLongestMatchWinsAndAllowBreaksTies(t *testing.T) {
	body := []byte("User-agent: *\nDisallow: /\nAllow: /members\nDisallow: /members/private\n")
	rules := parseRobots(body, ourAgent)

	cases := map[string]bool{
		"/":                     false,
		"/about":                false,
		"/members":              true,
		"/members/acme":         true,
		"/members/private":      false,
		"/members/private/list": false,
	}
	for path, want := range cases {
		if allowed, _ := rules.allows(path); allowed != want {
			t.Errorf("allows(%q) = %v, want %v", path, allowed, want)
		}
	}
}

func TestParseRobotsHonoursWildcardsAndAnchors(t *testing.T) {
	body := []byte("User-agent: *\nDisallow: /*/print$\nDisallow: /search?*\n")
	rules := parseRobots(body, ourAgent)

	cases := map[string]bool{
		"/members/print":       false,
		"/members/print/extra": true, // the "$" anchors the rule to the end of the path
		"/members":             true,
		"/search?q=acme":       false,
		"/searching":           true,
	}
	for path, want := range cases {
		if allowed, _ := rules.allows(path); allowed != want {
			t.Errorf("allows(%q) = %v, want %v", path, allowed, want)
		}
	}
}

func TestParseRobotsClampsAHostileCrawlDelay(t *testing.T) {
	rules := parseRobots([]byte("User-agent: *\nCrawl-delay: 99999\n"), ourAgent)
	if rules.crawlDelay > 5*time.Minute {
		t.Errorf("crawlDelay = %s, want it clamped: no run survives a delay that long",
			rules.crawlDelay)
	}
	if rules.crawlDelay == 0 {
		t.Error("crawlDelay = 0, want the clamped value: the site did ask for a delay")
	}
}

func TestMatchRobotsPathAnchoredSuffix(t *testing.T) {
	// "/*.pdf$" has to match a path whose *last* ".pdf" ends it, not the first one found.
	if !matchRobotsPath("/*.pdf$", "/files/a.pdf.pdf") {
		t.Error(`matchRobotsPath("/*.pdf$", "/files/a.pdf.pdf") = false, want true`)
	}
	if matchRobotsPath("/*.pdf$", "/files/a.pdf.html") {
		t.Error(`matchRobotsPath("/*.pdf$", "/files/a.pdf.html") = true, want false`)
	}
}
