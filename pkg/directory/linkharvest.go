package directory

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"

	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// LinkHarvestName is the adapter name for reading a member directory by its outbound links.
const LinkHarvestName = "link_harvest"

func init() { Register(LinkHarvestName, func() Adapter { return LinkHarvestAdapter{} }) }

// LinkHarvestAdapter reads a member directory without being told how it is built.
//
// It exists for the first day of a new source. Writing CSS selectors means opening the page
// and reading its markup; this needs only the URL, because a member directory's members are
// very largely the companies it links to. That makes a pasted URL useful immediately, and a
// source worth more precision can be upgraded to [HTMLDirectoryAdapter] later.
//
// What it cannot do is tell a member from a sponsor, a partner, or the agency that built
// the site — they are all outbound links on the same page. So it does not pretend
// otherwise: every record says in [model.DirectoryRecord.Incomplete] that it was harvested
// rather than read, and the result carries warnings with the counts behind it. A human
// reading the exported CSV can see which rows came from this adapter and judge them.
//
// Configuration:
//
//	adapter: link_harvest
//	access_method: html
//	directory_url: https://assoc.example.org/members
//	settings:
//	  container_selector: "ul.members"   # optional: skip the automatic scoping
//	  next_selector: "a[rel=next]"       # optional: follow pagination
//	  exclude_domains: [eventbrite.com]  # optional: beyond the built-in list
//	  min_links: 5                       # optional: a container must hold this many
//	  fail_on_empty_page: true           # default
type LinkHarvestAdapter struct{}

// Name implements [Adapter].
func (LinkHarvestAdapter) Name() string { return LinkHarvestName }

// notCompanies are domains that appear on almost every member directory and are never the
// member. Social networks, maps, document viewers, the usual plumbing.
var notCompanies = map[string]bool{
	"facebook.com": true, "twitter.com": true, "x.com": true, "linkedin.com": true,
	"instagram.com": true, "youtube.com": true, "youtu.be": true, "xing.com": true,
	"tiktok.com": true, "pinterest.com": true, "vimeo.com": true, "flickr.com": true,
	"google.com": true, "google.de": true, "google.fr": true, "goo.gl": true,
	"maps.google.com": true, "apple.com": true, "microsoft.com": true, "adobe.com": true,
	"wordpress.org": true, "wordpress.com": true, "wix.com": true, "squarespace.com": true,
	"typo3.org": true, "joomla.org": true, "drupal.org": true, "jimdo.com": true,
	"cookiebot.com": true, "onetrust.com": true, "usercentrics.com": true,
	"eventbrite.com": true, "mailchimp.com": true, "paypal.com": true, "gstatic.com": true,
	"cloudflare.com": true, "jquery.com": true, "bootstrapcdn.com": true, "w3.org": true,
	"europa.eu": true, "github.com": true, "gitlab.com": true,
}

// genericLabels are link texts that name the act of visiting rather than the company. A
// record whose only candidate name is one of these gets its domain as a name instead, and
// says so — inventing one would be worse, because the first name a company is seen under is
// the one it keeps.
var genericLabels = map[string]bool{
	"website": true, "web site": true, "webseite": true, "web": true, "www": true,
	"home": true, "homepage": true, "startseite": true, "site": true, "site web": true,
	"more": true, "mehr": true, "mehr erfahren": true, "read more": true, "weiterlesen": true,
	"details": true, "detail": true, "zur website": true, "visit": true, "besuchen": true,
	"link": true, "hier": true, "here": true, "info": true, "profil": true, "profile": true,
	"en savoir plus": true, "plus": true, "voir": true, "contact": true, "kontakt": true,
	"email": true, "e-mail": true, "mail": true, "anfahrt": true, "karte": true, "map": true,
}

// Discover reads the directory page and treats the companies it links to as its members.
func (LinkHarvestAdapter) Discover(ctx context.Context, req Request) (Result, error) {
	var result Result
	if req.Fetcher == nil {
		return result, fmt.Errorf("association %q: the %s adapter needs a fetcher; "+
			"access_method must be html, not manual", req.Association.Slug, LinkHarvestName)
	}
	start := strings.TrimSpace(req.Association.DirectoryURL)
	if start == "" {
		return result, fmt.Errorf("association %q: the %s adapter needs a directory_url",
			req.Association.Slug, LinkHarvestName)
	}

	settings := req.Association.Settings
	containerSelector := strings.TrimSpace(SettingString(settings, "container_selector", ""))
	nextSelector := strings.TrimSpace(SettingString(settings, "next_selector", "a[rel=next]"))
	minLinks := SettingInt(settings, "min_links", 3)
	failOnEmpty := true
	if raw, ok := settings["fail_on_empty_page"].(bool); ok {
		failOnEmpty = raw
	}
	excluded := map[string]bool{}
	for domain := range notCompanies {
		excluded[domain] = true
	}
	if extra, ok := settings["exclude_domains"].([]any); ok {
		for _, item := range extra {
			if text, ok := item.(string); ok {
				if domain := normalize.CanonicalDomain(text); domain != "" {
					excluded[domain] = true
				}
			}
		}
	}
	// A directory never lists itself, and its own domain is on every page of it.
	if own := normalize.CanonicalDomain(start); own != "" {
		excluded[own] = true
	}

	maxPages := req.Limits.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}

	found := map[string]*harvested{}
	var order []string
	pageURL := start
	seen := map[string]bool{}

	for pageNumber := 1; pageNumber <= maxPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if canonical := normalize.URL(pageURL); canonical != "" {
			if seen[canonical] {
				break
			}
			seen[canonical] = true
		}

		page, err := req.Fetcher.Get(ctx, pageURL)
		if err != nil {
			if reason, blocked := eucderrors.BlockedReason(err); blocked {
				result.Blocked = append(result.Blocked, Blocked{URL: pageURL, Reason: reason})
				break
			}
			if pageNumber == 1 {
				return result, fmt.Errorf("directory page %s: %w", pageURL, err)
			}
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("page %d (%s) could not be read: %v", pageNumber, pageURL, err))
			result.Truncated = true
			break
		}
		if !page.FromCache {
			result.PagesFetched++
		}

		document, err := goquery.NewDocumentFromReader(bytes.NewReader(page.Body))
		if err != nil {
			return result, fmt.Errorf("directory page %s is not parseable HTML: %w", page.URL, err)
		}

		links, total := harvestLinks(document, page, containerSelector, minLinks, excluded)
		for _, link := range links {
			existing, known := found[link.domain]
			if !known {
				copied := link
				found[link.domain] = &copied
				order = append(order, link.domain)
				continue
			}
			existing.count += link.count
			if betterName(link.name, existing.name) {
				existing.name = link.name
			}
		}
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"%s: %d outbound link(s), %d distinct company domain(s) after filtering%s",
			page.URL, total, len(links), containerNote(links)))

		next, hasNext := nextLink(document, page, nextSelector)
		if !hasNext {
			break
		}
		pageURL = next
		if pageNumber == maxPages {
			result.Truncated = true
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%s has a further page; stopped at max_pages (%d)", page.URL, maxPages))
		}
	}

	if len(order) == 0 {
		if failOnEmpty && len(result.Blocked) == 0 {
			return result, fmt.Errorf(
				"%s yielded no outbound company links at all. Either the member list is built "+
					"some way this cannot read, or the page needs a container_selector. "+
					"Refusing to report a successful run that found nothing",
				start)
		}
		return result, nil
	}

	retrievedAt := time.Now().UTC()
	sort.Strings(order)
	for _, domain := range order {
		link := found[domain]
		record := model.DirectoryRecord{
			RawName:     link.name,
			Country:     req.Association.Country,
			Website:     link.website,
			SourceURL:   start,
			RetrievedAt: retrievedAt,
			Sectors:     req.Association.Sectors,
			Extra: map[string]string{
				"harvested_from_links": "true",
				"link_count":           fmt.Sprint(link.count),
			},
		}
		// Every record says what it is. These were not read from a member list; they were
		// inferred from the links on a page that contains one.
		record.Incomplete = "harvested from the directory's outbound links, not read from a " +
			"member list: confirm it is a member and not a sponsor or supplier"
		if link.namedByDomain {
			record.Incomplete += "; the page gave no company name, so the domain is used"
		}
		if err := record.Validate(); err != nil {
			result.Warnings = append(result.Warnings, err.Error())
			continue
		}
		if req.Limits.MaxRecords > 0 && len(result.Records) >= req.Limits.MaxRecords {
			result.Truncated = true
			break
		}
		result.Records = append(result.Records, record)
	}
	return result, nil
}

// linkCandidate is one outbound link that survived filtering, kept with its DOM node so the
// page can be scoped to the block the links are densest in.
type linkCandidate struct {
	domain, website, name string
	node                  *html.Node
}

// harvested is one company domain and the best name seen for it.
type harvested struct {
	domain        string
	website       string
	name          string
	count         int
	namedByDomain bool
	container     string
}

// harvestLinks returns one entry per distinct company domain on the page, and how many
// outbound links there were in total.
func harvestLinks(
	document *goquery.Document, page *Page, containerSelector string, minLinks int,
	excluded map[string]bool,
) ([]harvested, int) {
	scope := document.Selection
	containerName := ""
	if containerSelector != "" {
		if chosen := document.Find(containerSelector); chosen.Length() > 0 {
			scope, containerName = chosen, containerSelector
		}
	}

	var candidates []linkCandidate
	scope.Find("a[href]").Each(func(_ int, anchor *goquery.Selection) {
		href, _ := anchor.Attr("href")
		absolute := resolveHref(page.URL, strings.TrimSpace(href))
		normalized := normalize.URL(absolute)
		if normalized == "" {
			return
		}
		domain := normalize.CanonicalDomain(normalized)
		if domain == "" || excluded[domain] {
			return
		}
		if len(anchor.Nodes) == 0 {
			return
		}
		candidates = append(candidates, linkCandidate{
			domain:  domain,
			website: normalized,
			name:    normalize.CollapseWhitespace(anchor.Text()),
			node:    anchor.Nodes[0],
		})
	})
	total := len(candidates)
	if total == 0 {
		return nil, 0
	}

	// Scope automatically to the smallest part of the page that holds most of the outbound
	// links. A member list is a dense block; a sponsor strip in the footer and a link to the
	// web agency are not. Doing this cuts most of the noise without anyone writing a
	// selector, and the container is reported so a reviewer can see what was chosen.
	if containerSelector == "" {
		kept, name := densestContainer(candidates, minLinks)
		if len(kept) > 0 {
			filtered := candidates[:0]
			for _, c := range candidates {
				if kept[c.node] {
					filtered = append(filtered, c)
				}
			}
			candidates, containerName = filtered, name
		}
	}

	byDomain := map[string]*harvested{}
	var order []string
	for _, c := range candidates {
		entry, known := byDomain[c.domain]
		if !known {
			entry = &harvested{domain: c.domain, website: c.website, container: containerName}
			byDomain[c.domain] = entry
			order = append(order, c.domain)
		}
		entry.count++
		if betterName(c.name, entry.name) {
			entry.name = c.name
		}
	}

	out := make([]harvested, 0, len(order))
	for _, domain := range order {
		entry := byDomain[domain]
		if entry.name == "" {
			// No usable name anywhere on the page. The domain is a fact; a guess would not
			// be, and the first name a company is seen under is the one it keeps.
			entry.name = entry.domain
			entry.namedByDomain = true
		}
		out = append(out, *entry)
	}
	return out, total
}

// densestContainer finds the tightest block of the page that still holds most of the
// outbound links — a member list is dense, a sponsor strip and a footer credit are not.
//
// Note what it does NOT do: pick the ancestor with the most links. That is always <body>,
// which is the whole page and scopes nothing. It takes the *deepest* ancestor still holding
// at least half of them, which is the member list itself.
func densestContainer(candidates []linkCandidate, minLinks int) (map[*html.Node]bool, string) {
	counts := map[*html.Node]int{}
	for _, c := range candidates {
		seen := map[*html.Node]bool{}
		for node := c.node.Parent; node != nil; node = node.Parent {
			if seen[node] {
				break
			}
			seen[node] = true
			counts[node]++
		}
	}

	threshold := (len(candidates) + 1) / 2
	if threshold < minLinks {
		threshold = minLinks
	}

	var best *html.Node
	bestDepth := -1
	for node, count := range counts {
		if count < threshold {
			continue
		}
		depth := 0
		for walk := node; walk != nil; walk = walk.Parent {
			depth++
		}
		// Deepest wins; on a tie the denser one does, which prefers a list over the
		// wrapper that merely contains it.
		if depth > bestDepth || (depth == bestDepth && best != nil && count > counts[best]) {
			best, bestDepth = node, depth
		}
	}
	if best == nil {
		return nil, ""
	}

	kept := map[*html.Node]bool{}
	for _, c := range candidates {
		for node := c.node.Parent; node != nil; node = node.Parent {
			if node == best {
				kept[c.node] = true
				break
			}
		}
	}
	return kept, describeNode(best)
}

// describeNode names a container the way a person would write a selector for it.
func describeNode(node *html.Node) string {
	if node == nil {
		return ""
	}
	name := node.Data
	for _, attribute := range node.Attr {
		switch attribute.Key {
		case "id":
			return name + "#" + attribute.Val
		case "class":
			if classes := strings.Fields(attribute.Val); len(classes) > 0 {
				name += "." + classes[0]
			}
		}
	}
	return name
}

func containerNote(links []harvested) string {
	if len(links) == 0 || links[0].container == "" {
		return ""
	}
	return fmt.Sprintf(", scoped to %q", links[0].container)
}

// betterName prefers a real company name over a generic label, and the longer of two real
// names — "Exemple Pharma SAS" over "Exemple".
func betterName(candidate, existing string) bool {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || isGenericLabel(candidate) {
		return false
	}
	if existing == "" || isGenericLabel(existing) {
		return true
	}
	return len(candidate) > len(existing)
}

func isGenericLabel(text string) bool {
	folded := strings.ToLower(strings.TrimSpace(normalize.StripAccents(text)))
	folded = strings.Trim(folded, " \t›»→>-–—:")
	return folded == "" || genericLabels[folded]
}

// nextLink finds a pagination link on the directory's own site.
func nextLink(document *goquery.Document, page *Page, selector string) (string, bool) {
	if selector == "" {
		return "", false
	}
	href, exists := document.Find(selector).First().Attr("href")
	if !exists || strings.TrimSpace(href) == "" {
		return "", false
	}
	next := resolveHref(page.URL, href)
	if !normalize.SameSite(next, page.URL) {
		return "", false
	}
	return next, true
}
