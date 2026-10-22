package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// Enrichment is what one visit to a company's own site established.
//
// It is deliberately not a [model.Company]: an enrichment run learns three kinds of thing —
// published contacts, a careers page, and what happened when we tried — and writing it
// through the general company upsert would re-run identity resolution on a company we
// already have in hand.
type Enrichment struct {
	// CareersURL is the careers page found, if any. It never overwrites one already known.
	CareersURL string
	// Description is the company's own words, read off one of its pages. It replaces a
	// shorter one and never a longer one, on the same reasoning as the company merge: a
	// later visit that happened to land on a thin page must not erase a richer earlier one.
	Description string
	// Contacts are published addresses read off a page. Each one names the page it came
	// from; the store refuses any that does not, and any that names an individual.
	Contacts []model.Contact
	// CrawlStatus is the outcome of this attempt, and replaces whatever was there: it
	// describes the latest visit, not the best one. A blocked or refused site must stay
	// visible, so it is recorded rather than left looking unvisited.
	CrawlStatus model.CrawlStatus
	// CrawlError is the detail behind a blocked, refused or failed status — the robots rule
	// that disallowed us, or the reason a URL was never requested.
	CrawlError string
	// CheckedAt is when the attempt happened. Zero means now.
	CheckedAt time.Time
}

// EnrichmentResult reports what was written, so a run can be summarised honestly.
type EnrichmentResult struct {
	// ContactsRecorded counts contacts that survived validation and were stored. It is not
	// the number of addresses found: addresses naming an individual are dropped, and
	// re-finding an address already known updates it rather than adding one.
	ContactsRecorded int
	// ContactsRejected counts contacts the store refused — personal addresses, or ones
	// claiming to be published without naming a page.
	ContactsRejected int
	// DescriptionSet is true when this visit stored the company's own words for the first
	// time, or replaced a thinner description with a fuller one.
	DescriptionSet bool
	// CareersURLSet is true when this run was the one that learned the careers page.
	CareersURLSet bool
}

// RecordEnrichment stores the outcome of one visit to a company's site.
//
// All of it in one transaction: a run killed between writing a contact and writing the
// crawl status would otherwise leave a company that has a contact and claims never to have
// been checked.
func (s *Store) RecordEnrichment(
	ctx context.Context, companyID string, enrichment Enrichment,
) (EnrichmentResult, error) {
	var result EnrichmentResult
	if companyID == "" {
		return result, fmt.Errorf("no company id given")
	}
	checkedAt := enrichment.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var existingCareers, existingQuality, existingDescription string
		err := tx.QueryRowContext(ctx,
			`SELECT careers_url, data_quality, description FROM companies WHERE company_id = ?`,
			companyID).Scan(&existingCareers, &existingQuality, &existingDescription)
		if err != nil {
			return fmt.Errorf("load company %s: %w", companyID, err)
		}

		published := false
		for _, contact := range enrichment.Contacts {
			// upsertContactTx is the second of the two layers that keep an individual's
			// address out of the database: extraction drops them, and this refuses them
			// again. It returns no error for a dropped address, so count what it kept.
			address := normalize.Email(contact.Address)
			if address == "" || normalize.LooksPersonal(address) {
				result.ContactsRejected++
				continue
			}
			// A claim with no evidence is refused, but it refuses one contact rather than
			// failing the whole visit.
			if err := contact.Validate(); err != nil {
				result.ContactsRejected++
				continue
			}
			if err := upsertContactTx(ctx, tx, companyID, contact, checkedAt); err != nil {
				return err
			}
			result.ContactsRecorded++
			if contact.Verification == model.VerificationPublished {
				published = true
			}
		}

		careers := existingCareers
		if careers == "" && enrichment.CareersURL != "" {
			if normalized := normalize.URL(enrichment.CareersURL); normalized != "" {
				careers = normalized
				result.CareersURLSet = true
			}
		}

		// A company whose site published a contact is as well evidenced as this engine can
		// make it. Quality only ever goes up: a later run that finds nothing must not
		// downgrade what an earlier one established.
		quality := model.Quality(existingQuality)
		if published {
			quality = bestQuality(quality, model.QualityHigh)
		}

		status := enrichment.CrawlStatus
		if status == "" {
			status = model.CrawlNotAttempted
		}

		// The longer text wins, matching the company merge: a later visit that landed on a
		// thin page must not erase a richer description an earlier one found.
		description := existingDescription
		if len(strings.TrimSpace(enrichment.Description)) > len(strings.TrimSpace(description)) {
			description = strings.TrimSpace(enrichment.Description)
			result.DescriptionSet = true
		}

		_, err = tx.ExecContext(ctx,
			`UPDATE companies SET careers_url = ?, data_quality = ?, crawl_status = ?,
			        crawl_error = ?, description = ?, last_checked_at = ?, updated_at = ?
			 WHERE company_id = ?`,
			careers, quality, status, enrichment.CrawlError, description,
			checkedAt.Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), companyID)
		if err != nil {
			return fmt.Errorf("update company %s after enrichment: %w", companyID, err)
		}
		return nil
	})
	return result, err
}
