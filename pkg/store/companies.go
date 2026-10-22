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
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// UpsertResult reports what an upsert actually did, so a run can be summarised honestly
// rather than claiming it "processed" a number of companies.
type UpsertResult struct {
	CompanyID string
	// Created is true when this call inserted a new company.
	Created bool
	// IdentitiesAdded counts identity keys newly pointing at this company — typically 1
	// when a run learns the website of a company it previously knew only by name.
	IdentitiesAdded int
	// Conflict is set when the record's domain and its name already belonged to two
	// different companies. The domain wins and nothing is merged; the text says what was
	// seen so a human can decide. See the comment in resolveIdentity.
	Conflict string
}

// UpsertCompany inserts or updates one company, resolving it to an existing record when the
// engine has seen it before.
//
// This is the function that makes "run discovery twice, get one company" true. Everything
// about it is shaped by one rule: join on exact identity, never on resemblance.
func (s *Store) UpsertCompany(ctx context.Context, candidate model.Company) (UpsertResult, error) {
	candidate.Name = normalize.DisplayName(candidate.Name)
	if candidate.Name == "" {
		return UpsertResult{}, errors.New("company has no name")
	}
	candidate.Country = strings.ToLower(strings.TrimSpace(candidate.Country))
	if candidate.Country == "" {
		return UpsertResult{}, fmt.Errorf("company %q has no country, so its name cannot identify it",
			candidate.Name)
	}
	candidate.NormalizedName = normalize.CompanyName(candidate.Name)
	if candidate.Website != "" {
		candidate.Website = normalize.URL(candidate.Website)
	}
	candidate.CanonicalDomain = normalize.CanonicalDomain(candidate.Website)
	if candidate.CareersURL != "" {
		candidate.CareersURL = normalize.URL(candidate.CareersURL)
	}

	var result UpsertResult
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		result, err = upsertCompanyTx(ctx, tx, candidate)
		return err
	})
	return result, err
}

func upsertCompanyTx(ctx context.Context, tx *sql.Tx, c model.Company) (UpsertResult, error) {
	now := time.Now().UTC()
	nameKey := normalize.NameIdentity(c.Name, c.Country)
	domainKey := normalize.DomainIdentity(c.Website)

	companyID, created, conflict, err := resolveIdentity(ctx, tx, c, nameKey, domainKey, now)
	if err != nil {
		return UpsertResult{}, err
	}
	result := UpsertResult{CompanyID: companyID, Created: created, Conflict: conflict}

	if !created {
		if err := mergeCompanyTx(ctx, tx, companyID, c, now); err != nil {
			return result, err
		}
	}

	// Attach both identities to this company. INSERT OR IGNORE matters: if another company
	// already owns one of these keys, it keeps it. Stealing an identity would silently
	// re-point history at the wrong company.
	for _, key := range []string{domainKey, nameKey} {
		if key == "" {
			continue
		}
		// The kind comes from the key's own prefix, which normalize.DomainIdentity and
		// normalize.NameIdentity decide: 'domain' is strong, because a registrable domain
		// belongs to one company; 'name' is weak, because it only holds within one country
		// and only after normalisation. One spelling of those values, in one package.
		kind, value, _ := strings.Cut(key, ":")
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO company_identities (kind, value, company_id, created_at)
			 VALUES (?, ?, ?, ?)`,
			kind, value, companyID, now.Format(time.RFC3339))
		if err != nil {
			return result, fmt.Errorf("record identity %q: %w", key, err)
		}
		if affected, _ := res.RowsAffected(); affected > 0 && !created {
			result.IdentitiesAdded++
		}
	}

	for _, source := range c.Sources {
		if err := upsertSourceTx(ctx, tx, companyID, source, now); err != nil {
			return result, err
		}
	}
	for _, contact := range c.Contacts {
		if err := upsertContactTx(ctx, tx, companyID, contact, now); err != nil {
			return result, err
		}
	}
	return result, nil
}

// resolveIdentity decides which company this record is, creating one if it is new.
func resolveIdentity(
	ctx context.Context, tx *sql.Tx, c model.Company, nameKey, domainKey string, now time.Time,
) (companyID string, created bool, conflict string, err error) {
	byDomain, err := lookupIdentity(ctx, tx, domainKey)
	if err != nil {
		return "", false, "", err
	}
	byName, err := lookupIdentity(ctx, tx, nameKey)
	if err != nil {
		return "", false, "", err
	}

	switch {
	case byDomain != "" && byName != "" && byDomain != byName:
		// Two companies already exist: one owns this domain, another owns this name in
		// this country. That is a real possibility — a holding company and a subsidiary
		// can share a website, and two unrelated firms can share a name. Merging them
		// would destroy information that cannot be recovered, so we attach to the domain
		// (the stronger signal) and report the collision for a human to look at.
		return byDomain, false, fmt.Sprintf(
			"domain %q belongs to company %s but the name %q in %q belongs to company %s; "+
				"attached to %s and merged nothing",
			normalize.CanonicalDomain(c.Website), byDomain, c.Name, c.Country, byName, byDomain), nil
	case byDomain != "":
		// Joining by domain is normally right, but a shared host makes it wrong: three
		// companies on subdomains of one domain, or on a site builder whose domain is not
		// in the public-suffix list, all reduce to the same registrable domain and would
		// become one company. A domain join between two names with nothing in common is
		// the signal for that, so it is reported rather than merged quietly.
		note, err := sharedDomainWarning(ctx, tx, byDomain, c)
		if err != nil {
			return "", false, "", err
		}
		return byDomain, false, note, nil
	case byName != "":
		return byName, false, "", nil
	}

	// New company. Its id comes from the strongest identity available, so a company first
	// seen with a website is keyed on the website.
	seed := domainKey
	if seed == "" {
		seed = nameKey
	}
	companyID = normalize.CompanyID(seed)
	if err := insertCompanyTx(ctx, tx, companyID, c, now); err != nil {
		return "", false, "", err
	}
	return companyID, true, "", nil
}

// sharedDomainWarning returns a non-empty note when joining c to companyID by domain looks
// like a shared-hosting mistake rather than a real match.
//
// The test is whether the two normalised names share any word. "Renault SAS" joining
// "RENAULT S.A.S." shares "renault" and is silent; "Exemple Injection" joining "Exemple
// Emboutissage" shares "exemple" and is silent too — which is the point, the test is
// deliberately weak so it only fires on names with genuinely nothing in common and does not
// cry wolf on every subsidiary.
func sharedDomainWarning(
	ctx context.Context, tx *sql.Tx, companyID string, c model.Company,
) (string, error) {
	var existingName string
	err := tx.QueryRowContext(ctx,
		`SELECT normalized_name FROM companies WHERE company_id = ?`, companyID).Scan(&existingName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("load company %s for the shared-domain check: %w", companyID, err)
	}
	incoming := normalize.CompanyName(c.Name)
	if incoming == existingName || shareAWord(incoming, existingName) {
		return "", nil
	}
	return fmt.Sprintf(
		"%q was joined to existing company %s (%q) because both resolve to the domain %q, "+
			"but the names have no word in common — check whether this is one company on one "+
			"site or several companies sharing a host",
		c.Name, companyID, existingName, normalize.CanonicalDomain(c.Website)), nil
}

func shareAWord(a, b string) bool {
	words := map[string]bool{}
	for _, word := range strings.Fields(a) {
		// Single letters and legal forms are too common to count as evidence.
		if len(word) > 2 && !legalForms[word] {
			words[word] = true
		}
	}
	for _, word := range strings.Fields(b) {
		if words[word] {
			return true
		}
	}
	return false
}

// legalForms are too widely shared to mean two names are related.
var legalForms = map[string]bool{
	"sas": true, "sarl": true, "sa": true, "sasu": true, "eurl": true, "snc": true,
	"gmbh": true, "ag": true, "kg": true, "ohg": true, "ug": true, "mbh": true,
	"bv": true, "nv": true, "spa": true, "srl": true, "ltd": true, "plc": true,
	"inc": true, "llc": true, "oy": true, "ab": true, "as": true, "aps": true,
	"group": true, "groupe": true, "holding": true, "holdings": true, "company": true,
	"international": true, "france": true, "deutschland": true, "europe": true,
}

func lookupIdentity(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	kind, value, _ := strings.Cut(key, ":")
	var companyID string
	err := tx.QueryRowContext(ctx,
		`SELECT company_id FROM company_identities WHERE kind = ? AND value = ?`,
		kind, value).Scan(&companyID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up identity %q: %w", key, err)
	}
	return companyID, nil
}

func insertCompanyTx(
	ctx context.Context, tx *sql.Tx, companyID string, c model.Company, now time.Time,
) error {
	quality := c.Quality
	if quality == "" {
		quality = model.QualityLow
	}
	crawlStatus := c.CrawlStatus
	if crawlStatus == "" {
		crawlStatus = model.CrawlNotAttempted
	}
	firstSeen := c.FirstSeenAt
	if firstSeen.IsZero() {
		firstSeen = now
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO companies (
		    company_id, name, normalized_name, country, region, website, canonical_domain,
		    description, size_bucket, languages_json, sectors_json, keywords_json,
		    careers_url, data_quality, crawl_status, crawl_error,
		    first_seen_at, last_checked_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		companyID, c.Name, c.NormalizedName, c.Country, c.Region, c.Website, c.CanonicalDomain,
		c.Description, c.SizeBucket, encodeJSON(c.Languages), encodeJSON(c.Sectors),
		encodeJSON(c.Keywords), c.CareersURL, quality, crawlStatus, c.CrawlError,
		firstSeen.Format(time.RFC3339), nullableTime(c.LastCheckedAt), now.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert company %q: %w", c.Name, err)
	}
	return nil
}

// mergeCompanyTx updates an existing company without losing what is already known.
//
// A later run often knows less than an earlier one — a directory listing gives a name and
// nothing else. Writing its empty fields over a website and a careers page discovered
// earlier would make the record worse with every pass, so empty values never overwrite.
// Lists are unioned for the same reason.
func mergeCompanyTx(
	ctx context.Context, tx *sql.Tx, companyID string, c model.Company, now time.Time,
) error {
	var existing model.Company
	var languages, sectors, keywords string
	var lastChecked sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT name, region, website, canonical_domain, description, size_bucket,
		        languages_json, sectors_json, keywords_json, careers_url,
		        data_quality, crawl_status, crawl_error, last_checked_at
		 FROM companies WHERE company_id = ?`, companyID).Scan(
		&existing.Name, &existing.Region, &existing.Website, &existing.CanonicalDomain,
		&existing.Description, &existing.SizeBucket, &languages, &sectors, &keywords,
		&existing.CareersURL, &existing.Quality, &existing.CrawlStatus, &existing.CrawlError,
		&lastChecked)
	if err != nil {
		return fmt.Errorf("load company %s for merge: %w", companyID, err)
	}

	website := preferNonEmpty(c.Website, existing.Website)
	merged := model.Company{
		Name:            preferNonEmpty(existing.Name, c.Name), // the first name seen is kept
		Region:          preferNonEmpty(c.Region, existing.Region),
		Website:         website,
		CanonicalDomain: normalize.CanonicalDomain(website),
		Description:     preferLonger(c.Description, existing.Description),
		SizeBucket:      preferNonEmpty(c.SizeBucket, existing.SizeBucket),
		Languages:       unionStrings(decodeJSON(languages), c.Languages),
		Sectors:         unionStrings(decodeJSON(sectors), c.Sectors),
		Keywords:        unionStrings(decodeJSON(keywords), c.Keywords),
		CareersURL:      preferNonEmpty(c.CareersURL, existing.CareersURL),
		Quality:         bestQuality(existing.Quality, c.Quality),
	}
	// Crawl state is the exception: it describes the latest attempt, so a fresh result
	// replaces the old one outright.
	merged.CrawlStatus, merged.CrawlError = existing.CrawlStatus, existing.CrawlError
	if c.CrawlStatus != "" && c.CrawlStatus != model.CrawlNotAttempted {
		merged.CrawlStatus, merged.CrawlError = c.CrawlStatus, c.CrawlError
	}
	checkedAt := lastChecked
	if c.LastCheckedAt != nil {
		checkedAt = sql.NullString{String: c.LastCheckedAt.Format(time.RFC3339), Valid: true}
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE companies SET
		    name = ?, normalized_name = ?, region = ?, website = ?, canonical_domain = ?,
		    description = ?, size_bucket = ?, languages_json = ?, sectors_json = ?,
		    keywords_json = ?, careers_url = ?, data_quality = ?, crawl_status = ?,
		    crawl_error = ?, last_checked_at = ?, updated_at = ?
		 WHERE company_id = ?`,
		merged.Name, normalize.CompanyName(merged.Name), merged.Region, merged.Website,
		merged.CanonicalDomain, merged.Description, merged.SizeBucket,
		encodeJSON(merged.Languages), encodeJSON(merged.Sectors), encodeJSON(merged.Keywords),
		merged.CareersURL, merged.Quality, merged.CrawlStatus, merged.CrawlError,
		checkedAt, now.Format(time.RFC3339), companyID)
	if err != nil {
		return fmt.Errorf("update company %s: %w", companyID, err)
	}
	return nil
}

func upsertSourceTx(
	ctx context.Context, tx *sql.Tx, companyID string, source model.Source, now time.Time,
) error {
	if source.SourceURL == "" || source.AssociationSlug == "" {
		return nil // a source we cannot name is not provenance
	}
	retrievedAt := source.RetrievedAt
	if retrievedAt.IsZero() {
		retrievedAt = now
	}
	// Seeing the same listing again refreshes when we last saw it, nothing else.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO company_sources (company_id, association_slug, source_url, raw_name, retrieved_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (company_id, association_slug, source_url)
		 DO UPDATE SET retrieved_at = excluded.retrieved_at`,
		companyID, source.AssociationSlug, normalize.URL(source.SourceURL),
		source.RawName, retrievedAt.Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("record source %q: %w", source.SourceURL, err)
	}
	return nil
}

func upsertContactTx(
	ctx context.Context, tx *sql.Tx, companyID string, contact model.Contact, now time.Time,
) error {
	address := normalize.Email(contact.Address)
	if address == "" {
		return nil
	}
	// The engine must never store an individual's address. This is enforced here as well as
	// at extraction time, so no future caller can get it wrong by accident.
	if normalize.LooksPersonal(address) {
		return nil
	}
	if err := contact.Validate(); err != nil {
		return err
	}
	kind := contact.Kind
	if kind == "" {
		kind = model.ContactKind(normalize.ClassifyLocalPart(address))
	}
	verification := contact.Verification
	if verification == "" {
		verification = model.VerificationUnverified
	}
	foundAt := contact.FoundAt
	if foundAt.IsZero() {
		foundAt = now
	}
	// Re-finding an address keeps the stronger evidence: a published sighting must never be
	// downgraded by a later unverified one.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO contacts (company_id, address, kind, verification, found_on_url, found_at, notes)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (company_id, address) DO UPDATE SET
		    kind = excluded.kind,
		    verification = CASE
		        WHEN contacts.verification = 'published' THEN contacts.verification
		        ELSE excluded.verification END,
		    found_on_url = CASE
		        WHEN contacts.verification = 'published' THEN contacts.found_on_url
		        ELSE excluded.found_on_url END,
		    found_at = excluded.found_at,
		    notes = excluded.notes`,
		companyID, address, kind, verification, contact.FoundOnURL,
		foundAt.Format(time.RFC3339), contact.Notes)
	if err != nil {
		return fmt.Errorf("record contact %q: %w", address, err)
	}
	return nil
}

// GetCompany loads one company with its contacts and sources.
func (s *Store) GetCompany(ctx context.Context, companyID string) (model.Company, error) {
	companies, err := s.loadCompanies(ctx,
		`SELECT company_id, name, normalized_name, country, region, website, canonical_domain,
		        description, size_bucket, languages_json, sectors_json, keywords_json,
		        careers_url, data_quality, crawl_status, crawl_error,
		        first_seen_at, last_checked_at, updated_at
		 FROM companies WHERE company_id = ?`, companyID)
	if err != nil {
		return model.Company{}, err
	}
	if len(companies) == 0 {
		return model.Company{}, fmt.Errorf("no company with id %q", companyID)
	}
	return companies[0], nil
}

// CompanyFilter narrows a listing.
type CompanyFilter struct {
	Country string
	Sector  string
	// WithContact restricts the result to companies that have at least one contact.
	WithContact bool
	Limit       int
	Offset      int
}

// ListCompanies returns companies matching filter, ordered by name for a stable listing.
func (s *Store) ListCompanies(ctx context.Context, filter CompanyFilter) ([]model.Company, error) {
	query := strings.Builder{}
	query.WriteString(
		`SELECT c.company_id, c.name, c.normalized_name, c.country, c.region, c.website,
		        c.canonical_domain, c.description, c.size_bucket, c.languages_json,
		        c.sectors_json, c.keywords_json, c.careers_url, c.data_quality,
		        c.crawl_status, c.crawl_error, c.first_seen_at, c.last_checked_at, c.updated_at
		 FROM companies c WHERE 1 = 1`)
	var args []any
	if filter.Country != "" {
		query.WriteString(" AND c.country = ?")
		args = append(args, strings.ToLower(filter.Country))
	}
	if filter.Sector != "" {
		// sectors_json is a JSON array of strings; matching the quoted form avoids
		// "auto" matching "automotive".
		query.WriteString(" AND c.sectors_json LIKE ?")
		args = append(args, "%\""+filter.Sector+"\"%")
	}
	if filter.WithContact {
		query.WriteString(" AND EXISTS (SELECT 1 FROM contacts ct WHERE ct.company_id = c.company_id)")
	}
	query.WriteString(" ORDER BY c.name COLLATE NOCASE, c.company_id")
	if filter.Limit > 0 {
		query.WriteString(" LIMIT ?")
		args = append(args, filter.Limit)
		if filter.Offset > 0 {
			query.WriteString(" OFFSET ?")
			args = append(args, filter.Offset)
		}
	}
	return s.loadCompanies(ctx, query.String(), args...)
}

func (s *Store) loadCompanies(
	ctx context.Context, query string, args ...any,
) ([]model.Company, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var companies []model.Company
	ids := make([]string, 0, 16)
	for rows.Next() {
		var c model.Company
		var languages, sectors, keywords string
		var firstSeen string
		var lastChecked sql.NullString
		var updated string
		if err := rows.Scan(
			&c.ID, &c.Name, &c.NormalizedName, &c.Country, &c.Region, &c.Website,
			&c.CanonicalDomain, &c.Description, &c.SizeBucket, &languages, &sectors,
			&keywords, &c.CareersURL, &c.Quality, &c.CrawlStatus, &c.CrawlError,
			&firstSeen, &lastChecked, &updated,
		); err != nil {
			return nil, err
		}
		c.Languages, c.Sectors, c.Keywords = decodeJSON(languages), decodeJSON(sectors), decodeJSON(keywords)
		c.FirstSeenAt = parseTime(firstSeen)
		c.UpdatedAt = parseTime(updated)
		if lastChecked.Valid {
			t := parseTime(lastChecked.String)
			c.LastCheckedAt = &t
		}
		companies = append(companies, c)
		ids = append(ids, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(companies) == 0 {
		return companies, nil
	}

	contacts, err := s.contactsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	sources, err := s.sourcesFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range companies {
		companies[i].Contacts = contacts[companies[i].ID]
		companies[i].Sources = sources[companies[i].ID]
	}
	return companies, nil
}

func (s *Store) contactsFor(
	ctx context.Context, companyIDs []string,
) (map[string][]model.Contact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT company_id, address, kind, verification, found_on_url, found_at, notes
		 FROM contacts WHERE company_id IN (`+placeholders(len(companyIDs))+`)
		 ORDER BY company_id, kind, address`, toAny(companyIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.Contact{}
	for rows.Next() {
		var companyID, foundAt string
		var contact model.Contact
		if err := rows.Scan(&companyID, &contact.Address, &contact.Kind, &contact.Verification,
			&contact.FoundOnURL, &foundAt, &contact.Notes); err != nil {
			return nil, err
		}
		contact.FoundAt = parseTime(foundAt)
		out[companyID] = append(out[companyID], contact)
	}
	return out, rows.Err()
}

func (s *Store) sourcesFor(
	ctx context.Context, companyIDs []string,
) (map[string][]model.Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT company_id, association_slug, source_url, raw_name, retrieved_at
		 FROM company_sources WHERE company_id IN (`+placeholders(len(companyIDs))+`)
		 ORDER BY company_id, association_slug, source_url`, toAny(companyIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.Source{}
	for rows.Next() {
		var companyID, retrievedAt string
		var source model.Source
		if err := rows.Scan(&companyID, &source.AssociationSlug, &source.SourceURL,
			&source.RawName, &retrievedAt); err != nil {
			return nil, err
		}
		source.RetrievedAt = parseTime(retrievedAt)
		out[companyID] = append(out[companyID], source)
	}
	return out, rows.Err()
}

// CountCompanies returns how many companies match filter, ignoring Limit and Offset.
func (s *Store) CountCompanies(ctx context.Context, filter CompanyFilter) (int, error) {
	filter.Limit, filter.Offset = 0, 0
	companies, err := s.ListCompanies(ctx, filter)
	if err != nil {
		return 0, err
	}
	return len(companies), nil
}

// ---------------------------------------------------------------- helpers

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback() //nolint:errcheck // the original error is what matters
		return err
	}
	return tx.Commit()
}

func encodeJSON(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

func decodeJSON(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	return values
}

func parseTime(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func nullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.Format(time.RFC3339)
}

func preferNonEmpty(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

// preferLonger keeps the more informative of two descriptions. A directory blurb and a
// homepage summary are both valid; the longer one usually says more.
func preferLonger(a, b string) string {
	if len(strings.TrimSpace(a)) >= len(strings.TrimSpace(b)) {
		return a
	}
	return b
}

func unionStrings(existing, incoming []string) []string {
	seen := make(map[string]bool, len(existing)+len(incoming))
	out := make([]string, 0, len(existing)+len(incoming))
	for _, group := range [][]string{existing, incoming} {
		for _, value := range group {
			value = strings.TrimSpace(value)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

var qualityRank = map[model.Quality]int{
	model.QualityLow: 0, model.QualityMedium: 1, model.QualityHigh: 2,
}

func bestQuality(a, b model.Quality) model.Quality {
	if qualityRank[b] > qualityRank[a] {
		return b
	}
	if a == "" {
		return model.QualityLow
	}
	return a
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
