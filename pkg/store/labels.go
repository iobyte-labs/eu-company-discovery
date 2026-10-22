package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// UpsertLabel records one judgement about a company, replacing any earlier answer to the
// same question.
//
// Deliberately narrow: it writes to company_labels and to nothing else. A judgement must
// never reach companies.sectors, companies.data_quality or contacts, because a consumer
// reads those as things that were observed. Keeping the write path separate is what makes
// that a property of the code rather than a rule someone has to remember.
func (s *Store) UpsertLabel(ctx context.Context, companyID string, label model.Label) error {
	if strings.TrimSpace(companyID) == "" {
		return fmt.Errorf("no company id given")
	}
	if err := label.Validate(); err != nil {
		return err
	}
	createdAt := label.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO company_labels (company_id, kind, label, reason, source, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (company_id, kind) DO UPDATE SET
		    label = excluded.label, reason = excluded.reason,
		    source = excluded.source, created_at = excluded.created_at`,
		companyID, label.Kind, label.Label, label.Reason, label.Source,
		createdAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("record label %q for %s: %w", label.Kind, companyID, err)
	}
	return nil
}

// DeleteLabels removes every label of one kind. Judgements are disposable by design: if a
// model turns out to have been wrong across the board, the answer is to throw away its
// answers and ask again, which is not something that could be done to a fact.
func (s *Store) DeleteLabels(ctx context.Context, kind string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM company_labels WHERE kind = ?`, kind)
	if err != nil {
		return 0, fmt.Errorf("delete labels of kind %q: %w", kind, err)
	}
	return result.RowsAffected()
}

func (s *Store) labelsFor(ctx context.Context, companyIDs []string) (map[string][]model.Label, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT company_id, kind, label, reason, source, created_at
		 FROM company_labels WHERE company_id IN (`+placeholders(len(companyIDs))+`)
		 ORDER BY company_id, kind`, toAny(companyIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]model.Label{}
	for rows.Next() {
		var companyID, createdAt string
		var label model.Label
		if err := rows.Scan(&companyID, &label.Kind, &label.Label, &label.Reason,
			&label.Source, &createdAt); err != nil {
			return nil, err
		}
		label.CreatedAt = parseTime(createdAt)
		out[companyID] = append(out[companyID], label)
	}
	return out, rows.Err()
}

// restoreLabelsTx writes a company's labels back on a restore, so rebuilding the database
// from an export does not silently throw away judgements that cost money to produce.
func restoreLabelsTx(ctx context.Context, tx *sql.Tx, companyID string, labels []model.Label) error {
	for _, label := range labels {
		if err := label.Validate(); err != nil {
			return fmt.Errorf("company %s: %w", companyID, err)
		}
		createdAt := label.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO company_labels
			    (company_id, kind, label, reason, source, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			companyID, label.Kind, label.Label, label.Reason, label.Source,
			createdAt.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("restore label %q for %s: %w", label.Kind, companyID, err)
		}
	}
	return nil
}
