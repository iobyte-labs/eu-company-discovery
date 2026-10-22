package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// UpsertProfile stores a profile.
//
// Profiles live in config/; this table exists because company_rankings.profile_slug has a
// foreign key to it, so a score cannot be written for a profile the database has never
// seen. Storing the payload alongside also records which version of a profile produced a
// given score, which is what makes an old ranking interpretable.
func (s *Store) UpsertProfile(ctx context.Context, profile model.Profile) error {
	if strings.TrimSpace(profile.Slug) == "" {
		return errors.New("profile has no slug")
	}
	payload, err := json.Marshal(profile)
	if err != nil {
		return fmt.Errorf("encode profile %q: %w", profile.Slug, err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO professional_profiles (slug, payload_json, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT (slug) DO UPDATE SET
		    payload_json = excluded.payload_json,
		    updated_at = excluded.updated_at`,
		profile.Slug, string(payload), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("store profile %q: %w", profile.Slug, err)
	}
	return nil
}

// GetProfile loads a stored profile.
func (s *Store) GetProfile(ctx context.Context, slug string) (model.Profile, error) {
	var payload string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload_json FROM professional_profiles WHERE slug = ?`, slug).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Profile{}, fmt.Errorf("no profile %q is stored", slug)
	}
	if err != nil {
		return model.Profile{}, err
	}
	var profile model.Profile
	if err := json.Unmarshal([]byte(payload), &profile); err != nil {
		return model.Profile{}, fmt.Errorf("decode profile %q: %w", slug, err)
	}
	return profile, nil
}

// ListProfiles returns every stored profile slug, sorted.
func (s *Store) ListProfiles(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT slug FROM professional_profiles ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// RankingWriteResult reports what a write actually did.
type RankingWriteResult struct {
	Written int
	Removed int
}

// ReplaceRankings stores a complete set of rankings for one profile, removing any previous
// ones first.
//
// Use this after scoring the whole corpus: the table then holds exactly what the current
// profile says, so a company that now vetoes cannot linger at the score it had last week.
// For a filtered run use PutRankings instead, or the rest of the table is lost.
func (s *Store) ReplaceRankings(
	ctx context.Context, profileSlug string, rankings []model.Ranking,
) (RankingWriteResult, error) {
	var result RankingWriteResult
	if err := validateRankings(rankings); err != nil {
		return result, err
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		deleted, err := tx.ExecContext(ctx,
			`DELETE FROM company_rankings WHERE profile_slug = ?`, profileSlug)
		if err != nil {
			return fmt.Errorf("clear rankings for %q: %w", profileSlug, err)
		}
		if removed, err := deleted.RowsAffected(); err == nil {
			result.Removed = int(removed)
		}
		written, err := insertRankings(ctx, tx, rankings)
		result.Written = written
		return err
	})
	return result, err
}

// PutRankings stores rankings without touching any others. For a filtered run.
func (s *Store) PutRankings(ctx context.Context, rankings []model.Ranking) (int, error) {
	if err := validateRankings(rankings); err != nil {
		return 0, err
	}
	written := 0
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		written, err = insertRankings(ctx, tx, rankings)
		return err
	})
	return written, err
}

// validateRankings enforces the rule that gives a score its worth.
//
// reasons_json is NOT NULL, but "[]" would satisfy that. A score nobody can account for is
// exactly what this project treats as a bug, so the store refuses it rather than trusting
// every caller to remember — the same two-layer discipline as the personal-address refusal.
func validateRankings(rankings []model.Ranking) error {
	for _, ranking := range rankings {
		if ranking.CompanyID == "" {
			return errors.New("a ranking names no company")
		}
		if ranking.ProfileSlug == "" {
			return fmt.Errorf("the ranking for %s names no profile", ranking.CompanyID)
		}
		if len(ranking.Reasons) == 0 {
			return fmt.Errorf("the ranking for %s carries no reasons; a score that cannot "+
				"say where it came from is not reviewable", ranking.CompanyID)
		}
	}
	return nil
}

func insertRankings(ctx context.Context, tx *sql.Tx, rankings []model.Ranking) (int, error) {
	statement, err := tx.PrepareContext(ctx,
		`INSERT INTO company_rankings
		    (company_id, profile_slug, score, reasons_json, computed_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (company_id, profile_slug) DO UPDATE SET
		    score = excluded.score,
		    reasons_json = excluded.reasons_json,
		    computed_at = excluded.computed_at`)
	if err != nil {
		return 0, err
	}
	defer statement.Close()

	written := 0
	for _, ranking := range rankings {
		reasons, err := json.Marshal(ranking.Reasons)
		if err != nil {
			return written, fmt.Errorf("encode reasons for %s: %w", ranking.CompanyID, err)
		}
		computedAt := ranking.ComputedAt
		if computedAt.IsZero() {
			computedAt = time.Now().UTC()
		}
		if _, err := statement.ExecContext(ctx, ranking.CompanyID, ranking.ProfileSlug,
			ranking.Score, string(reasons), computedAt.Format(time.RFC3339)); err != nil {
			return written, fmt.Errorf("store ranking for %s: %w", ranking.CompanyID, err)
		}
		written++
	}
	return written, nil
}

// RankingFilter narrows a ranked listing.
type RankingFilter struct {
	ProfileSlug string // required
	MinScore    float64
	// ExcludeZero drops vetoed and gated companies, which score exactly zero.
	ExcludeZero bool
	Country     string
	Sector      string
	WithContact bool
	Limit       int
	Offset      int
}

// ScoredCompany pairs a company with its ranking.
type ScoredCompany struct {
	Company model.Company `json:"company"`
	Ranking model.Ranking `json:"ranking"`
}

// TopCompanies returns companies ordered by score, highest first.
func (s *Store) TopCompanies(ctx context.Context, f RankingFilter) ([]ScoredCompany, error) {
	if f.ProfileSlug == "" {
		return nil, errors.New("TopCompanies needs a profile slug")
	}

	query := strings.Builder{}
	// Ordering matches idx_company_rankings_score (profile_slug, score DESC), which is why
	// that index exists.
	query.WriteString(
		`SELECT r.company_id, r.score, r.reasons_json, r.computed_at
		 FROM company_rankings r
		 JOIN companies c ON c.company_id = r.company_id
		 WHERE r.profile_slug = ?`)
	args := []any{f.ProfileSlug}

	if f.MinScore > 0 {
		query.WriteString(" AND r.score >= ?")
		args = append(args, f.MinScore)
	}
	if f.ExcludeZero {
		query.WriteString(" AND r.score > 0")
	}
	if f.Country != "" {
		query.WriteString(" AND c.country = ?")
		args = append(args, strings.ToLower(f.Country))
	}
	if f.Sector != "" {
		query.WriteString(" AND c.sectors_json LIKE ?")
		args = append(args, "%\""+f.Sector+"\"%")
	}
	if f.WithContact {
		query.WriteString(
			" AND EXISTS (SELECT 1 FROM contacts ct WHERE ct.company_id = c.company_id)")
	}
	query.WriteString(" ORDER BY r.score DESC, r.company_id")
	if f.Limit > 0 {
		query.WriteString(" LIMIT ?")
		args = append(args, f.Limit)
		if f.Offset > 0 {
			query.WriteString(" OFFSET ?")
			args = append(args, f.Offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rankings []model.Ranking
	ids := make([]string, 0, 32)
	for rows.Next() {
		var ranking model.Ranking
		var reasons, computedAt string
		if err := rows.Scan(&ranking.CompanyID, &ranking.Score, &reasons, &computedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(reasons), &ranking.Reasons); err != nil {
			return nil, fmt.Errorf("decode reasons for %s: %w", ranking.CompanyID, err)
		}
		ranking.ProfileSlug = f.ProfileSlug
		ranking.ComputedAt = parseTime(computedAt)
		rankings = append(rankings, ranking)
		ids = append(ids, ranking.CompanyID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(rankings) == 0 {
		return nil, nil
	}

	// Hydrate through loadCompanies so contacts and sources come back the same way they do
	// everywhere else; a second copy of that logic would drift.
	companies, err := s.loadCompanies(ctx,
		`SELECT company_id, name, normalized_name, country, region, website, canonical_domain,
		        description, size_bucket, languages_json, sectors_json, keywords_json,
		        careers_url, data_quality, crawl_status, crawl_error,
		        first_seen_at, last_checked_at, updated_at
		 FROM companies WHERE company_id IN (`+placeholders(len(ids))+`)`, toAny(ids)...)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]model.Company, len(companies))
	for _, company := range companies {
		byID[company.ID] = company
	}

	// SQL IN does not preserve order, so the ranked order is restored here deliberately.
	out := make([]ScoredCompany, 0, len(rankings))
	for _, ranking := range rankings {
		company, ok := byID[ranking.CompanyID]
		if !ok {
			continue
		}
		out = append(out, ScoredCompany{Company: company, Ranking: ranking})
	}
	return out, nil
}

// CountRankings reports how many rankings are stored for a profile.
func (s *Store) CountRankings(ctx context.Context, profileSlug string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM company_rankings WHERE profile_slug = ?`, profileSlug).Scan(&count)
	return count, err
}
