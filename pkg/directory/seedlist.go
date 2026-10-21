package directory

import (
	"context"
	"fmt"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// SeedListName is the adapter name for a hand-curated list in configuration.
const SeedListName = "seed_list"

func init() { Register(SeedListName, func() Adapter { return SeedListAdapter{} }) }

// SeedListAdapter reads companies written directly into an association's YAML file.
//
// It fetches nothing. It exists for the cases where automated discovery is the wrong tool:
// a directory that is a PDF, one that refuses automated access, an association that
// publishes nothing machine-readable, or a handful of companies an operator already knows.
// Having a first-class way to say "these came from a person" keeps curated entries inside
// the same identity, dedup and provenance machinery as crawled ones, instead of being
// pasted into the database by hand.
//
// Configuration:
//
//	adapter: seed_list
//	access_method: manual
//	settings:
//	  source_note: "read from the 2026 member yearbook, p. 44"
//	  companies:
//	    - name: Acme Industries SAS
//	      website: https://acme.fr        # optional
//	      region: idf                     # optional
//	      description: ...                # optional
//	      sectors: [automotive]           # optional, defaults to the association's
//	      source_url: https://assoc.fr/members.pdf   # optional, defaults to directory_url
type SeedListAdapter struct{}

// Name implements [Adapter].
func (SeedListAdapter) Name() string { return SeedListName }

// Discover converts the configured entries into directory records.
func (SeedListAdapter) Discover(_ context.Context, req Request) (Result, error) {
	var result Result

	entries, ok := Setting[[]any](req.Association.Settings, "companies")
	if !ok {
		return result, fmt.Errorf(
			"association %q uses the %s adapter but its settings have no `companies:` list",
			req.Association.Slug, SeedListName)
	}
	if len(entries) == 0 {
		return result, fmt.Errorf("association %q has an empty `companies:` list",
			req.Association.Slug)
	}

	// Every curated record points at the same declared origin, so a reader can still see
	// where the list came from. Falling back to the directory URL keeps provenance
	// non-empty even when no per-entry source is given.
	defaultSource := req.Association.DirectoryURL
	if note := SettingString(req.Association.Settings, "source_url", ""); note != "" {
		defaultSource = note
	}
	if defaultSource == "" {
		defaultSource = "config:" + req.Association.Slug
	}

	retrievedAt := time.Now().UTC()
	maxRecords := req.Limits.MaxRecords

	for index, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("entry %d is not a mapping; skipped", index+1))
			continue
		}
		name := SettingString(entry, "name", "")
		if name == "" {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("entry %d has no `name:`; skipped", index+1))
			continue
		}

		record := model.DirectoryRecord{
			RawName:     normalize.DisplayName(name),
			Country:     SettingString(entry, "country", req.Association.Country),
			Region:      SettingString(entry, "region", ""),
			Website:     SettingString(entry, "website", ""),
			SourceURL:   SettingString(entry, "source_url", defaultSource),
			RetrievedAt: retrievedAt,
			Description: SettingString(entry, "description", ""),
			Sectors:     stringList(entry, "sectors", req.Association.Sectors),
			Keywords:    stringList(entry, "keywords", nil),
		}
		if note := SettingString(req.Association.Settings, "source_note", ""); note != "" {
			record.Extra = map[string]string{"source_note": note}
		}
		// A curated website is still only a claim until it is checked, exactly as a
		// crawled one is. Saying so here stops the record looking better evidenced than it
		// is.
		if record.Website != "" && normalize.URL(record.Website) == "" {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%s: website %q is not an http(s) url; dropped",
					record.RawName, record.Website))
			record.Website = ""
		}
		if record.Website == "" {
			record.Incomplete = "no website in the curated entry"
		}
		if err := record.Validate(); err != nil {
			result.Warnings = append(result.Warnings, err.Error())
			continue
		}

		result.Records = append(result.Records, record)
		if maxRecords > 0 && len(result.Records) >= maxRecords && index+1 < len(entries) {
			result.Truncated = true
			break
		}
	}

	if len(result.Records) == 0 {
		return result, fmt.Errorf("association %q: none of the %d curated entries were usable",
			req.Association.Slug, len(entries))
	}
	return result, nil
}

// stringList reads a YAML list of strings, falling back when it is absent.
func stringList(entry map[string]any, key string, fallback []string) []string {
	raw, ok := entry[key]
	if !ok {
		return fallback
	}
	switch typed := raw.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		if len(out) == 0 {
			return fallback
		}
		return out
	case []string:
		return typed
	case string:
		return []string{typed}
	default:
		return fallback
	}
}
