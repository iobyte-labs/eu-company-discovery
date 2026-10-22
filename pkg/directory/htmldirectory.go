package directory

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"

	eucderrors "github.com/iobyte-labs/eu-company-discovery/pkg/errors"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// HTMLDirectoryName is the adapter name for a member directory read from HTML with CSS
// selectors written in configuration.
const HTMLDirectoryName = "html_directory"

// DefaultMaxPages bounds pagination when an association sets no max_pages. A pagination
// loop has to terminate even when nobody remembered to say where.
const DefaultMaxPages = 25

func init() { Register(HTMLDirectoryName, func() Adapter { return HTMLDirectoryAdapter{} }) }

// HTMLDirectoryAdapter reads a member directory described entirely by configuration.
//
// Most public member directories are one repeating block per member, so most new sources
// need no code at all: a row selector, a selector per field, and a pagination rule. That is
// what "adding a source is configuration, not a code change" means in practice, and it is
// the route to reach for before writing an adapter.
//
// Configuration:
//
//	adapter: html_directory
//	access_method: html
//	directory_url: https://assoc.example.org/members
//	limits:
//	  max_pages: 25
//	  requests_per_minute: 10
//	settings:
//	  row_selector: "ul.member-list > li.member"
//	  fields:
//	    name: "h3.member-name"               # a bare string selects the element's text
//	    website:
//	      selector: "a.member-site"          # a mapping can read an attribute instead
//	      attribute: href
//	    region: "span.member-region"
//	    description: "p.member-summary"
//	    membership: "span.member-class"      # an unrecognised name is kept in Extra
//	  pagination:
//	    mode: next_link                      # follow a "next" link
//	    next_selector: "a[rel=next]"
//	    # mode: page_param                   # or walk ?page=2, ?page=3, …
//	    # param: page
//	    # max: 12
//	  fail_on_empty_page: true
//
// fail_on_empty_page is the setting that matters most. A listing page that yields no rows
// means the site changed its markup, and a run that reports success with zero companies
// hides that for weeks — so by default it is a failure, loudly, rather than an empty
// result.
type HTMLDirectoryAdapter struct{}

// Name implements [Adapter].
func (HTMLDirectoryAdapter) Name() string { return HTMLDirectoryName }

// paginationMode is how the adapter walks from one listing page to the next.
type paginationMode string

const (
	// paginationNone reads exactly one page.
	paginationNone paginationMode = ""
	// paginationNextLink follows a link on the page, which is the common case and the only
	// one that cannot overshoot the end of the directory.
	paginationNextLink paginationMode = "next_link"
	// paginationPageParam appends an incrementing query parameter. It needs `max`, because
	// nothing on the page says where the directory ends.
	paginationPageParam paginationMode = "page_param"
)

// htmlField is one field's selector, and optionally the attribute to read instead of text.
type htmlField struct {
	name      string
	selector  string
	attribute string
}

type htmlSettings struct {
	rowSelector     string
	fields          []htmlField
	mode            paginationMode
	nextSelector    string
	param           string
	maxParamPages   int
	failOnEmptyPage bool
}

// recognisedFields map a configured field name onto a [model.DirectoryRecord] field.
// Anything else a directory publishes — a membership class, a staff count — is kept in
// Extra rather than being dropped, because the engine cannot know in advance what will turn
// out to be worth having.
var recognisedFields = map[string]bool{
	"name": true, "website": true, "region": true, "country": true, "description": true,
}

// parseHTMLSettings reads and checks an association's settings, reporting every problem it
// finds in one message. A crawl that dies on the second mistake after a fix wastes an
// afternoon.
func parseHTMLSettings(association model.Association) (htmlSettings, error) {
	settings := association.Settings
	var problems []string
	out := htmlSettings{
		rowSelector:     strings.TrimSpace(SettingString(settings, "row_selector", "")),
		failOnEmptyPage: true,
	}
	if out.rowSelector == "" {
		problems = append(problems,
			"settings.row_selector is required: it selects the repeating block that is one member")
	} else if _, err := cascadia.Compile(out.rowSelector); err != nil {
		problems = append(problems, fmt.Sprintf("settings.row_selector %q is not a CSS selector: %v",
			out.rowSelector, err))
	}

	if raw, ok := settings["fail_on_empty_page"]; ok {
		flag, isBool := raw.(bool)
		if !isBool {
			problems = append(problems, "settings.fail_on_empty_page must be true or false")
		} else {
			out.failOnEmptyPage = flag
		}
	}

	fields, hadFields := settings["fields"]
	if !hadFields {
		problems = append(problems, "settings.fields is required: at least a `name:` selector")
	} else {
		parsed, fieldProblems := parseHTMLFields(fields)
		out.fields = parsed
		problems = append(problems, fieldProblems...)
	}

	if raw, ok := settings["pagination"]; ok {
		problems = append(problems, out.parsePagination(raw)...)
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return htmlSettings{}, fmt.Errorf("association %q: %s",
			association.Slug, strings.Join(problems, "; "))
	}
	return out, nil
}

func parseHTMLFields(raw any) ([]htmlField, []string) {
	mapping, ok := raw.(map[string]any)
	if !ok {
		return nil, []string{"settings.fields must be a mapping of field name to selector"}
	}
	var problems []string
	names := make([]string, 0, len(mapping))
	for name := range mapping {
		names = append(names, name)
	}
	// Sorted so that a field-ordering change in YAML cannot change a record's Extra, and so
	// error messages come out in a stable order.
	sort.Strings(names)

	fields := make([]htmlField, 0, len(names))
	for _, name := range names {
		field := htmlField{name: name}
		switch typed := mapping[name].(type) {
		case string:
			field.selector = strings.TrimSpace(typed)
		case map[string]any:
			field.selector = strings.TrimSpace(SettingString(typed, "selector", ""))
			field.attribute = strings.TrimSpace(SettingString(typed, "attribute", ""))
		default:
			problems = append(problems, fmt.Sprintf(
				"settings.fields.%s must be a selector string or a {selector, attribute} mapping",
				name))
			continue
		}
		if field.selector == "" {
			problems = append(problems, fmt.Sprintf("settings.fields.%s has no selector", name))
			continue
		}
		if _, err := cascadia.Compile(field.selector); err != nil {
			problems = append(problems, fmt.Sprintf("settings.fields.%s: %q is not a CSS selector: %v",
				name, field.selector, err))
			continue
		}
		fields = append(fields, field)
	}
	if !mappingHasField(fields, "name") {
		problems = append(problems, "settings.fields needs a `name:` selector; "+
			"a record with no company name is not admissible")
	}
	return fields, problems
}

func mappingHasField(fields []htmlField, name string) bool {
	for _, field := range fields {
		if field.name == name {
			return true
		}
	}
	return false
}

func (s *htmlSettings) parsePagination(raw any) []string {
	mapping, ok := raw.(map[string]any)
	if !ok {
		return []string{"settings.pagination must be a mapping with a `mode:`"}
	}
	var problems []string
	s.mode = paginationMode(strings.TrimSpace(SettingString(mapping, "mode", "")))
	switch s.mode {
	case paginationNone:
		problems = append(problems, "settings.pagination needs a mode: next_link or page_param")
	case paginationNextLink:
		s.nextSelector = strings.TrimSpace(SettingString(mapping, "next_selector", ""))
		if s.nextSelector == "" {
			problems = append(problems,
				"settings.pagination.next_selector is required for mode next_link")
		} else if _, err := cascadia.Compile(s.nextSelector); err != nil {
			problems = append(problems, fmt.Sprintf(
				"settings.pagination.next_selector %q is not a CSS selector: %v",
				s.nextSelector, err))
		}
	case paginationPageParam:
		s.param = strings.TrimSpace(SettingString(mapping, "param", ""))
		if s.param == "" {
			problems = append(problems, "settings.pagination.param is required for mode page_param")
		}
		s.maxParamPages = SettingInt(mapping, "max", 0)
		if s.maxParamPages <= 0 {
			problems = append(problems, "settings.pagination.max is required for mode page_param: "+
				"nothing on the page says where the directory ends")
		}
	default:
		problems = append(problems, fmt.Sprintf(
			"settings.pagination.mode %q is not next_link or page_param", s.mode))
	}
	return problems
}

// Discover reads the directory one listing page at a time.
func (HTMLDirectoryAdapter) Discover(ctx context.Context, req Request) (Result, error) {
	var result Result
	if req.Fetcher == nil {
		return result, fmt.Errorf("association %q: the %s adapter needs a fetcher; "+
			"access_method must be html, not manual", req.Association.Slug, HTMLDirectoryName)
	}
	start := strings.TrimSpace(req.Association.DirectoryURL)
	if start == "" {
		return result, fmt.Errorf("association %q: the %s adapter needs a directory_url",
			req.Association.Slug, HTMLDirectoryName)
	}
	settings, err := parseHTMLSettings(req.Association)
	if err != nil {
		return result, err
	}

	maxPages := req.Limits.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}
	if settings.mode == paginationPageParam && settings.maxParamPages < maxPages {
		maxPages = settings.maxParamPages
	}

	pageURL := start
	seen := map[string]bool{}
	for pageNumber := 1; pageNumber <= maxPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if settings.mode == paginationPageParam && pageNumber > 1 {
			if pageURL, err = withPageParam(start, settings.param, pageNumber); err != nil {
				return result, err
			}
		}
		// A "next" link pointing back into the chain would otherwise walk until max_pages,
		// refetching the same two pages.
		if canonical := normalize.URL(pageURL); canonical != "" {
			if seen[canonical] {
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"pagination returned to %s, which was already read; stopping", canonical))
				break
			}
			seen[canonical] = true
		}

		page, err := req.Fetcher.Get(ctx, pageURL)
		if err != nil {
			if reason, blocked := eucderrors.BlockedReason(err); blocked {
				// Reported and left alone. Never a second attempt, never a workaround.
				result.Blocked = append(result.Blocked, Blocked{URL: pageURL, Reason: reason})
				break
			}
			if pageNumber == 1 {
				// Nothing could be read at all, which is the one case an adapter reports as
				// an error rather than as a partial result.
				return result, fmt.Errorf("listing page %s: %w", pageURL, err)
			}
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("listing page %d (%s) could not be read: %v", pageNumber, pageURL, err))
			result.Truncated = true
			break
		}
		if !page.FromCache {
			result.PagesFetched++
		}

		document, err := goquery.NewDocumentFromReader(bytes.NewReader(page.Body))
		if err != nil {
			return result, fmt.Errorf("listing page %s is not parseable HTML: %w", page.URL, err)
		}

		rows := document.Find(settings.rowSelector)
		if rows.Length() == 0 {
			if settings.failOnEmptyPage {
				return result, fmt.Errorf(
					"listing page %s matched no rows for row_selector %q: the site's markup has "+
						"changed, or the selector is wrong. %d record(s) were read before this "+
						"page; refusing to report a successful run that found nothing",
					page.URL, settings.rowSelector, len(result.Records))
			}
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"listing page %s matched no rows for row_selector %q; stopping",
				page.URL, settings.rowSelector))
			break
		}

		pageRecords, warnings := recordsFromRows(rows, page, settings, req.Association)
		result.Warnings = append(result.Warnings, warnings...)
		for _, record := range pageRecords {
			if req.Limits.MaxRecords > 0 && len(result.Records) >= req.Limits.MaxRecords {
				result.Truncated = true
				return result, nil
			}
			result.Records = append(result.Records, record)
		}

		next, err := nextPageURL(document, page, settings, pageNumber)
		if err != nil {
			result.Warnings = append(result.Warnings, err.Error())
			break
		}
		if next == "" {
			return result, nil
		}
		if settings.mode == paginationNextLink {
			pageURL = next
		}
		if pageNumber == maxPages {
			// There was another page and we are not reading it. Saying so is the difference
			// between a partial result and a wrong one.
			result.Truncated = true
		}
	}
	return result, nil
}

// recordsFromRows turns one listing page's rows into records.
func recordsFromRows(
	rows *goquery.Selection, page *Page, settings htmlSettings, association model.Association,
) ([]model.DirectoryRecord, []string) {
	records := make([]model.DirectoryRecord, 0, rows.Length())
	var warnings []string

	rows.Each(func(index int, row *goquery.Selection) {
		record := model.DirectoryRecord{
			Country:     association.Country,
			SourceURL:   page.URL,
			RetrievedAt: page.FetchedAt,
			Sectors:     association.Sectors,
		}
		if record.RetrievedAt.IsZero() {
			record.RetrievedAt = time.Now().UTC()
		}
		var missing []string
		for _, field := range settings.fields {
			value := fieldValue(row, field)
			if value == "" {
				if recognisedFields[field.name] {
					missing = append(missing, field.name)
				}
				continue
			}
			switch field.name {
			case "name":
				record.RawName = normalize.DisplayName(value)
			case "website":
				record.Website = resolveHref(page.URL, value)
			case "region":
				record.Region = normalize.CollapseWhitespace(value)
			case "country":
				record.Country = strings.ToLower(normalize.CollapseWhitespace(value))
			case "description":
				record.Description = normalize.CollapseWhitespace(value)
			default:
				if record.Extra == nil {
					record.Extra = map[string]string{}
				}
				record.Extra[field.name] = normalize.CollapseWhitespace(value)
			}
		}

		if record.RawName == "" {
			warnings = append(warnings, fmt.Sprintf(
				"row %d on %s matched the row selector but has no name; skipped",
				index+1, page.URL))
			return
		}
		// A website that is not an http(s) URL is dropped rather than stored: a record is
		// allowed to be incomplete, but not to carry a value that looks like evidence and
		// is not.
		if record.Website != "" && normalize.URL(record.Website) == "" {
			warnings = append(warnings, fmt.Sprintf("%s: website %q is not an http(s) url; dropped",
				record.RawName, record.Website))
			record.Website = ""
			missing = append(missing, "website")
		}
		// Report what the directory did not give us. An adapter reports rather than
		// discards, so a thin row is still a record and the gap is visible.
		if len(missing) > 0 {
			sort.Strings(missing)
			record.Incomplete = "the listing gave no " + strings.Join(missing, ", ")
		}
		if err := record.Validate(); err != nil {
			warnings = append(warnings, err.Error())
			return
		}
		records = append(records, record)
	})
	return records, warnings
}

// fieldValue reads one field out of a row: an attribute when the configuration names one,
// otherwise the element's text.
func fieldValue(row *goquery.Selection, field htmlField) string {
	selection := row.Find(field.selector)
	if selection.Length() == 0 {
		// A field selector may also name the row itself, e.g. fields.name on an <a> row.
		if row.Is(field.selector) {
			selection = row
		} else {
			return ""
		}
	}
	if field.attribute != "" {
		value, _ := selection.First().Attr(field.attribute)
		return strings.TrimSpace(value)
	}
	return normalize.CollapseWhitespace(selection.First().Text())
}

// resolveHref turns a relative link into an absolute one against the page it was read from.
func resolveHref(pageURL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return href
	}
	resolved, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(resolved).String()
}

// nextPageURL finds the next listing page, or "" when this was the last one.
func nextPageURL(
	document *goquery.Document, page *Page, settings htmlSettings, pageNumber int,
) (string, error) {
	switch settings.mode {
	case paginationNextLink:
		href, exists := document.Find(settings.nextSelector).First().Attr("href")
		if !exists || strings.TrimSpace(href) == "" {
			return "", nil
		}
		next := resolveHref(page.URL, href)
		// A "next" link that leaves the directory's own site is not pagination. Following
		// it would turn a bounded read of one directory into an open crawl of the web.
		if !normalize.SameSite(next, page.URL) {
			return "", fmt.Errorf("the next-page link on %s points at %s, which is a different "+
				"site; stopping rather than following it", page.URL, next)
		}
		return next, nil
	case paginationPageParam:
		// Nothing on the page says where the directory ends, so `max` does. The caller
		// builds the next URL from the page number; a non-empty value here only says
		// "there may be another page".
		if pageNumber >= settings.maxParamPages {
			return "", nil
		}
		return "next", nil
	default:
		return "", nil
	}
}

// withPageParam adds or replaces the pagination query parameter on the directory URL.
func withPageParam(base, param string, pageNumber int) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("directory_url %q will not parse: %w", base, err)
	}
	query := parsed.Query()
	query.Set(param, fmt.Sprint(pageNumber))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
