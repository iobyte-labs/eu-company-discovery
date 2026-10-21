// Package export writes and reads the versioned data contract between this engine and
// whatever consumes it.
//
// This file is the engine's public promise. A private consumer reads these exports and must
// not need the engine's source, its database or its git history to do so — so the format is
// self-describing: it states its schema version, when it was generated and by what, and it
// carries each company's provenance with it.
//
// Changing the shape in a way that could break a reader means bumping
// [model.ExportSchemaVersion]. Adding an optional field does not: readers ignore unknown
// fields, which is why [Read] does not reject them.
package export

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// Envelope wraps exported companies with everything a reader needs to interpret them.
type Envelope struct {
	// SchemaVersion is [model.ExportSchemaVersion] at the time of writing.
	SchemaVersion string    `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	// Generator names the tool and version that produced the file, so a surprising export
	// can be traced to a build.
	Generator string `json:"generator"`
	// Filter records the selection this export represents, so a partial export is never
	// mistaken for the whole database.
	Filter map[string]string `json:"filter,omitempty"`
	Count  int               `json:"count"`
	// Companies is the payload. Sorted by company_id so two exports of the same data are
	// byte-identical and diffable.
	Companies []model.Company `json:"companies"`
}

// ErrSchemaMismatch is returned when a file's schema version is not one this build reads.
var ErrSchemaMismatch = errors.New("export schema version mismatch")

// Write serialises an envelope as indented JSON.
//
// Indented on purpose: these files get committed, reviewed and diffed, and a one-line JSON
// blob makes a reviewer's job impossible. The cost is some bytes.
func Write(w io.Writer, envelope Envelope) error {
	if envelope.SchemaVersion == "" {
		envelope.SchemaVersion = model.ExportSchemaVersion
	}
	if envelope.GeneratedAt.IsZero() {
		envelope.GeneratedAt = time.Now().UTC()
	}
	envelope.Count = len(envelope.Companies)
	sort.Slice(envelope.Companies, func(i, j int) bool {
		return envelope.Companies[i].ID < envelope.Companies[j].ID
	})

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	// Company descriptions and names come off other people's web pages and routinely
	// contain characters HTML-escaping would mangle.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return fmt.Errorf("write export: %w", err)
	}
	return nil
}

// Read parses an envelope and checks that this build understands its schema.
//
// A reader that silently accepts an unknown version is worse than one that refuses: the
// consumer would act on fields it has misunderstood. The error says what to do about it.
func Read(r io.Reader) (Envelope, error) {
	var envelope Envelope
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(&envelope); err != nil {
		return envelope, fmt.Errorf("read export: %w", err)
	}
	if envelope.SchemaVersion == "" {
		return envelope, fmt.Errorf(
			"%w: the file states no schema_version, so it was not written by this engine",
			ErrSchemaMismatch)
	}
	if !Compatible(envelope.SchemaVersion) {
		return envelope, fmt.Errorf(
			"%w: file is %q but this build reads %q — regenerate the export with a matching "+
				"engine version, or upgrade the consumer",
			ErrSchemaMismatch, envelope.SchemaVersion, model.ExportSchemaVersion)
	}
	return envelope, nil
}

// Compatible reports whether this build can read the given schema version.
//
// Versions are "<name>/<major>". A reader accepts its own major version only: within a
// major version, fields are added but never removed or re-meant.
func Compatible(schemaVersion string) bool {
	wantName, wantMajor, wantOK := strings.Cut(model.ExportSchemaVersion, "/")
	gotName, gotMajor, gotOK := strings.Cut(schemaVersion, "/")
	if !wantOK || !gotOK {
		return schemaVersion == model.ExportSchemaVersion
	}
	return wantName == gotName && wantMajor == gotMajor
}

// csvColumns is the flat view. CSV exists for spreadsheets and quick eyeballing; it cannot
// represent several contacts or several sources per company, so it carries the best contact
// and counts for the rest, and says as much in docs/data-contract.md. JSON is the contract.
var csvColumns = []string{
	"company_id", "name", "country", "region", "website", "canonical_domain",
	"careers_url", "sectors", "best_contact", "best_contact_kind",
	"best_contact_verification", "best_contact_source", "contact_count",
	"data_quality", "crawl_status", "association_slugs", "source_count",
	"first_seen_at", "last_checked_at",
}

// WriteCSV writes a flattened view for human review.
func WriteCSV(w io.Writer, companies []model.Company) error {
	writer := csv.NewWriter(w)
	if err := writer.Write(csvColumns); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	sorted := append([]model.Company(nil), companies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	for _, company := range sorted {
		best, hasContact := company.BestContact()
		associations := make([]string, 0, len(company.Sources))
		seen := map[string]bool{}
		for _, source := range company.Sources {
			if !seen[source.AssociationSlug] {
				seen[source.AssociationSlug] = true
				associations = append(associations, source.AssociationSlug)
			}
		}
		sort.Strings(associations)

		row := []string{
			company.ID, company.Name, company.Country, company.Region, company.Website,
			company.CanonicalDomain, company.CareersURL, strings.Join(company.Sectors, "|"),
			"", "", "", "", fmt.Sprint(len(company.Contacts)),
			string(company.Quality), string(company.CrawlStatus),
			strings.Join(associations, "|"), fmt.Sprint(len(company.Sources)),
			formatTime(company.FirstSeenAt), formatTimePtr(company.LastCheckedAt),
		}
		if hasContact {
			row[8], row[9] = best.Address, string(best.Kind)
			row[10], row[11] = string(best.Verification), best.FoundOnURL
		}
		if err := writer.Write(row); err != nil {
			return fmt.Errorf("write csv row for %q: %w", company.Name, err)
		}
	}
	writer.Flush()
	return writer.Error()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}
