package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// RestoreResult reports what a restore wrote.
type RestoreResult struct {
	Companies int
	Contacts  int
	Sources   int
	// Labels counts judgements carried over.
	Labels int
	// Identities counts the identity keys attached. It is normally more than Companies,
	// because a company with a website has both a name key and a domain key.
	Identities int
}

// RestoreCompanies writes companies back exactly as given, preserving their ids.
//
// This is a second write path into the same tables as [Store.UpsertCompany], and it exists
// for one reason: a company's id is derived from the *first* identity it was ever seen
// under, and that derivation cannot be run backwards. A company first seen by name, whose
// website was learned later, has an id derived from the name while its row now carries
// both — so re-deriving the id on the way in would rename it, and every reference to it
// elsewhere would break. The id therefore arrives verbatim and is written verbatim.
//
// Use it to rebuild a database from this engine's own export. Do not use it for data
// arriving from outside: that has no id of ours and belongs in UpsertCompany, which
// resolves identity properly.
func (s *Store) RestoreCompanies(
	ctx context.Context, companies []model.Company,
) (RestoreResult, error) {
	var result RestoreResult
	if err := checkRestorable(companies); err != nil {
		return result, err
	}

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, company := range companies {
			written, err := restoreCompanyTx(ctx, tx, company)
			if err != nil {
				return err
			}
			result.Companies++
			result.Contacts += written.Contacts
			result.Sources += written.Sources
			result.Identities += written.Identities
			result.Labels += written.Labels
		}
		return nil
	})
	return result, err
}

// checkRestorable refuses a file that cannot be restored without inventing or destroying
// something. Every one of these is a refusal rather than a repair, because the alternative
// is a database that looks fine and is quietly wrong.
func checkRestorable(companies []model.Company) error {
	var problems []string
	seenID := map[string]string{}
	seenDomain := map[string]string{}

	for _, company := range companies {
		switch {
		case strings.TrimSpace(company.ID) == "":
			problems = append(problems, fmt.Sprintf("company %q has no company_id", company.Name))
			continue
		case strings.TrimSpace(company.Name) == "":
			problems = append(problems, fmt.Sprintf("company %s has no name", company.ID))
		case strings.TrimSpace(company.Country) == "":
			problems = append(problems,
				fmt.Sprintf("company %q has no country, so its name cannot identify it", company.Name))
		}
		if previous, clash := seenID[company.ID]; clash {
			problems = append(problems, fmt.Sprintf(
				"company_id %s appears twice, as %q and %q", company.ID, previous, company.Name))
			continue
		}
		seenID[company.ID] = company.Name

		// The export carries each company's canonical_domain but not which company *owns*
		// that domain identity. Where the engine deliberately refused to merge two records
		// sharing a domain — a holding company and a subsidiary on one website, or two
		// firms on a shared host — a restore would resolve both to one owner and silently
		// collapse exactly the records a human was meant to look at. So it stops instead.
		if domain := company.CanonicalDomain; domain != "" {
			if previous, clash := seenDomain[domain]; clash {
				problems = append(problems, fmt.Sprintf(
					"companies %q and %q both claim the domain %q; restoring would merge them. "+
						"Resolve it in the source file before restoring",
					previous, company.Name, domain))
				continue
			}
			seenDomain[domain] = company.Name
		}
		// A missing timestamp would otherwise be silently replaced with "now", turning a
		// company first seen two years ago into one first seen today.
		if company.FirstSeenAt.IsZero() {
			problems = append(problems,
				fmt.Sprintf("company %q has no first_seen_at", company.Name))
		}
		for _, contact := range company.Contacts {
			if err := contact.Validate(); err != nil {
				problems = append(problems, fmt.Sprintf("company %q: %v", company.Name, err))
			}
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		if len(problems) == 1 {
			return fmt.Errorf("the dataset cannot be restored: %s", problems[0])
		}
		return fmt.Errorf("the dataset cannot be restored (%d problems):\n  - %s",
			len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

type restoreCounts struct{ Contacts, Sources, Identities, Labels int }

func restoreCompanyTx(
	ctx context.Context, tx *sql.Tx, c model.Company,
) (restoreCounts, error) {
	var counts restoreCounts
	now := time.Now().UTC()

	normalizedName := c.NormalizedName
	if normalizedName == "" {
		normalizedName = normalize.CompanyName(c.Name)
	}
	quality := c.Quality
	if quality == "" {
		quality = model.QualityLow
	}
	crawlStatus := c.CrawlStatus
	if crawlStatus == "" {
		crawlStatus = model.CrawlNotAttempted
	}
	updatedAt := c.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}

	// INSERT OR REPLACE so a restore over an existing database is idempotent: restoring the
	// same file twice leaves the same rows, and restoring a newer file replaces them.
	_, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO companies (
		    company_id, name, normalized_name, country, region, website, canonical_domain,
		    description, size_bucket, languages_json, sectors_json, keywords_json,
		    careers_url, data_quality, crawl_status, crawl_error,
		    first_seen_at, last_checked_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, normalizedName, c.Country, c.Region, c.Website, c.CanonicalDomain,
		c.Description, c.SizeBucket, encodeJSON(c.Languages), encodeJSON(c.Sectors),
		encodeJSON(c.Keywords), c.CareersURL, quality, crawlStatus, c.CrawlError,
		c.FirstSeenAt.Format(time.RFC3339), nullableTime(c.LastCheckedAt),
		updatedAt.Format(time.RFC3339))
	if err != nil {
		return counts, fmt.Errorf("restore company %q: %w", c.Name, err)
	}

	// The same two identity keys UpsertCompany would attach, derived from the row rather
	// than invented. INSERT OR IGNORE keeps an existing owner, so a restore can never steal
	// an identity from a company that already holds it.
	for _, key := range []string{normalize.DomainIdentity(c.Website), normalize.NameIdentity(c.Name, c.Country)} {
		if key == "" {
			continue
		}
		kind, value, _ := strings.Cut(key, ":")
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO company_identities (kind, value, company_id, created_at)
			 VALUES (?, ?, ?, ?)`,
			kind, value, c.ID, c.FirstSeenAt.Format(time.RFC3339))
		if err != nil {
			return counts, fmt.Errorf("restore identity %q: %w", key, err)
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			counts.Identities++
		}
	}

	for _, source := range c.Sources {
		if err := upsertSourceTx(ctx, tx, c.ID, source, now); err != nil {
			return counts, err
		}
		counts.Sources++
	}
	for _, contact := range c.Contacts {
		// upsertContactTx refuses an individual's address, as it does on every other path
		// into this table. A restore is not an exception to that promise.
		if err := upsertContactTx(ctx, tx, c.ID, contact, now); err != nil {
			return counts, err
		}
		counts.Contacts++
	}
	// Judgements are carried too. They are disposable in principle, but they cost money to
	// produce, and a cache miss should not quietly spend it again.
	if err := restoreLabelsTx(ctx, tx, c.ID, c.Labels); err != nil {
		return counts, err
	}
	counts.Labels += len(c.Labels)
	return counts, nil
}
