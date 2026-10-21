// Package model holds the engine's normalized data types.
//
// These types are the contract between every other package: adapters produce
// [DirectoryRecord]s, the store persists [Company] and [Contact], the exporter writes
// them out under a versioned envelope. Nothing here reads a CV, writes an application or
// sends mail — that belongs to a private consumer of this library.
//
// Every type carries its provenance. A company knows which directory listed it and when;
// a contact knows the page it was read from and whether that page published it or whether
// we inferred it. Discovery that cannot say where a fact came from is not useful, because
// nobody can check it.
package model

import (
	"fmt"
	"strings"
	"time"
)

// ExportSchemaVersion identifies the shape of exported JSON. Bump it only when a consumer
// would have to change to keep reading the output. See pkg/export.
const ExportSchemaVersion = "eucd.companies/1"

// ContactKind says what a published address is *for*. It is read from the address and the
// page around it, never from a guess about a person.
type ContactKind string

const (
	// ContactRecruitment is an address published for applications (jobs@, recrutement@).
	ContactRecruitment ContactKind = "recruitment"
	// ContactHR is a human-resources address that is not specifically for applications.
	ContactHR ContactKind = "hr"
	// ContactGeneral is a company's general inbox (contact@, info@). A fallback that
	// wants a human decision before anything is sent to it.
	ContactGeneral ContactKind = "general"
	// ContactOther is a published functional address with no recognised purpose.
	ContactOther ContactKind = "other"
)

// Verification records how strongly a contact is evidenced. The engine only ever writes
// [VerificationPublished]; the weaker values exist so a consumer that infers an address
// can store it without it being mistaken for something we read off a page.
type Verification string

const (
	// VerificationPublished means the address appeared, in full, on a page we fetched.
	// FoundOnURL points at that page.
	VerificationPublished Verification = "published"
	// VerificationInferred means it was constructed from a pattern, not read. The engine
	// never produces these.
	VerificationInferred Verification = "inferred"
	// VerificationUnverified means it came from outside and has not been checked.
	VerificationUnverified Verification = "unverified"
)

// Quality is a coarse confidence in a company record as a whole.
type Quality string

const (
	// QualityHigh means an official website was confirmed and a contact was published.
	QualityHigh Quality = "high"
	// QualityMedium means the website is known but contacts are thin or generic.
	QualityMedium Quality = "medium"
	// QualityLow means little more than a name from a directory.
	QualityLow Quality = "low"
)

// CrawlStatus is the outcome of the last attempt to visit a company's site. Failures are
// kept, not dropped: a source that blocks automated access must stay visible so it can be
// reported rather than quietly retried forever.
type CrawlStatus string

const (
	CrawlNotAttempted CrawlStatus = "not_attempted"
	CrawlOK           CrawlStatus = "ok"
	// CrawlBlocked means the site refuses automated access — robots, a login wall, a
	// CAPTCHA or a hard 403. Never worked around. See docs/compliance.md.
	CrawlBlocked CrawlStatus = "blocked"
	CrawlError   CrawlStatus = "error"
	// CrawlRefused means our own safety rules rejected the URL before any request.
	CrawlRefused CrawlStatus = "refused"
)

// AccessMethod is how an association's directory may be read. It is declared in the
// association's config so that an adapter can never quietly escalate to a method the
// operator did not accept.
type AccessMethod string

const (
	// AccessHTML is ordinary rate-limited fetching of public HTML pages.
	AccessHTML AccessMethod = "html"
	// AccessAPI is a documented public API or structured-data endpoint.
	AccessAPI AccessMethod = "api"
	// AccessDataset is an official downloadable CSV/JSON/XML directory.
	AccessDataset AccessMethod = "dataset"
	// AccessManual is a hand-curated seed list committed to config. No fetching.
	AccessManual AccessMethod = "manual"
)

// Association is a configured source of company listings.
type Association struct {
	Slug         string            `json:"slug"`
	Name         string            `json:"name"`
	Country      string            `json:"country"`
	Sectors      []string          `json:"sectors,omitempty"`
	DirectoryURL string            `json:"directory_url,omitempty"`
	Adapter      string            `json:"adapter"`
	AccessMethod AccessMethod      `json:"access_method"`
	Settings     map[string]any    `json:"settings,omitempty"`
	Limits       CrawlLimits       `json:"limits"`
	Notes        string            `json:"notes,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// CrawlLimits bounds what one discovery run may do to one source. Zero values mean "use
// the engine default", so a config file only states the limits it actually wants to change.
type CrawlLimits struct {
	MaxPages        int           `json:"max_pages,omitempty"`
	MaxRecords      int           `json:"max_records,omitempty"`
	RequestsPerMin  float64       `json:"requests_per_minute,omitempty"`
	Timeout         time.Duration `json:"timeout,omitempty"`
	MaxResponseSize int64         `json:"max_response_bytes,omitempty"`
}

// DirectoryRecord is what an adapter yields: one row of a member directory, as read, with
// no interpretation beyond trimming. Turning these into companies is the engine's job, not
// the adapter's, so that every adapter benefits from one identity and dedup policy.
type DirectoryRecord struct {
	// RawName is the company name exactly as the directory printed it.
	RawName string `json:"raw_name"`
	// Country is ISO 3166-1 alpha-2, lowercase. Defaults to the association's country.
	Country string `json:"country,omitempty"`
	Region  string `json:"region,omitempty"`
	// Website is the link the directory gave, if any. Treated as a claim to be checked,
	// not as the company's confirmed site.
	Website string `json:"website,omitempty"`
	// SourceURL is the directory page this row was read from. Required: a record whose
	// origin we cannot name is not admissible.
	SourceURL   string            `json:"source_url"`
	RetrievedAt time.Time         `json:"retrieved_at"`
	Description string            `json:"description,omitempty"`
	Sectors     []string          `json:"sectors,omitempty"`
	Keywords    []string          `json:"keywords,omitempty"`
	Extra       map[string]string `json:"extra,omitempty"`
	// Incomplete explains what the directory did not give us, when a row is partial but
	// still worth keeping. An adapter reports rather than discards.
	Incomplete string `json:"incomplete,omitempty"`
}

// Validate reports whether a record carries the minimum to be admissible.
func (r DirectoryRecord) Validate() error {
	if strings.TrimSpace(r.RawName) == "" {
		return fmt.Errorf("record has no name (source %q)", r.SourceURL)
	}
	if strings.TrimSpace(r.SourceURL) == "" {
		return fmt.Errorf("record %q has no source URL", r.RawName)
	}
	if r.RetrievedAt.IsZero() {
		return fmt.Errorf("record %q has no retrieval timestamp", r.RawName)
	}
	return nil
}

// Contact is one published address for a company.
type Contact struct {
	Address      string       `json:"address"`
	Kind         ContactKind  `json:"kind"`
	Verification Verification `json:"verification"`
	// FoundOnURL is the page the address was read from. Mandatory for published contacts.
	FoundOnURL string    `json:"found_on_url,omitempty"`
	FoundAt    time.Time `json:"found_at"`
	Notes      string    `json:"notes,omitempty"`
}

// Validate enforces the rule that gives a contact its worth: if we claim a page published
// it, we must be able to name the page.
func (c Contact) Validate() error {
	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("contact has no address")
	}
	if c.Verification == VerificationPublished && strings.TrimSpace(c.FoundOnURL) == "" {
		return fmt.Errorf("contact %q is marked published but names no page", c.Address)
	}
	return nil
}

// Source is one directory listing that produced or confirmed a company.
type Source struct {
	AssociationSlug string    `json:"association_slug"`
	SourceURL       string    `json:"source_url"`
	RawName         string    `json:"raw_name,omitempty"`
	RetrievedAt     time.Time `json:"retrieved_at"`
}

// Company is a discovered company after normalisation and deduplication.
//
// ID is assigned by the store from the first identity the company was seen under and never
// changes afterwards. Learning the company's domain later adds an identity pointing at the
// same ID rather than renaming it, which is what makes repeated runs idempotent.
type Company struct {
	ID              string      `json:"company_id"`
	Name            string      `json:"name"`
	NormalizedName  string      `json:"normalized_name"`
	Country         string      `json:"country"`
	Region          string      `json:"region,omitempty"`
	Website         string      `json:"website,omitempty"`
	CanonicalDomain string      `json:"canonical_domain,omitempty"`
	Description     string      `json:"description,omitempty"`
	SizeBucket      string      `json:"size_bucket,omitempty"`
	Languages       []string    `json:"languages,omitempty"`
	Sectors         []string    `json:"sectors,omitempty"`
	Keywords        []string    `json:"keywords,omitempty"`
	CareersURL      string      `json:"careers_url,omitempty"`
	Contacts        []Contact   `json:"contacts,omitempty"`
	Sources         []Source    `json:"sources,omitempty"`
	Quality         Quality     `json:"data_quality"`
	CrawlStatus     CrawlStatus `json:"crawl_status"`
	CrawlError      string      `json:"crawl_error,omitempty"`
	FirstSeenAt     time.Time   `json:"first_seen_at"`
	LastCheckedAt   *time.Time  `json:"last_checked_at,omitempty"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

// BestContact returns the contact most appropriate to approach, and whether one exists.
// Recruitment beats HR beats general; an unverified address never wins over a published
// one. A general inbox is returned, but a caller is expected to treat it as a fallback
// needing review rather than as an invitation.
func (c Company) BestContact() (Contact, bool) {
	rank := map[ContactKind]int{
		ContactRecruitment: 0,
		ContactHR:          1,
		ContactGeneral:     2,
		ContactOther:       3,
	}
	best, found := Contact{}, false
	for _, candidate := range c.Contacts {
		if candidate.Address == "" {
			continue
		}
		if !found {
			best, found = candidate, true
			continue
		}
		if contactBetter(candidate, best, rank) {
			best = candidate
		}
	}
	return best, found
}

func contactBetter(a, b Contact, rank map[ContactKind]int) bool {
	aPublished := a.Verification == VerificationPublished
	bPublished := b.Verification == VerificationPublished
	if aPublished != bPublished {
		return aPublished
	}
	ra, okA := rank[a.Kind]
	if !okA {
		ra = len(rank)
	}
	rb, okB := rank[b.Kind]
	if !okB {
		rb = len(rank)
	}
	return ra < rb
}

// CrawlRun records one execution of a discovery or enrichment job, so that a failed or
// partial run can be inspected afterwards instead of being guessed at.
type CrawlRun struct {
	ID              int64          `json:"id"`
	Kind            string         `json:"kind"`
	AssociationSlug string         `json:"association_slug,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	FinishedAt      *time.Time     `json:"finished_at,omitempty"`
	Status          string         `json:"status"`
	Stats           map[string]int `json:"stats,omitempty"`
	Error           string         `json:"error,omitempty"`
}

// SkillWeight is one scored requirement in a profile. Weight may be negative, which is how
// a profile says "this makes a company less relevant to me".
type SkillWeight struct {
	// Term is matched case- and accent-insensitively against company text.
	Term string `json:"term"`
	// Weight contributes to the score when Term matches. Negative weights subtract.
	Weight float64 `json:"weight"`
	// Aliases are other spellings and local-language terms for the same thing.
	Aliases []string `json:"aliases,omitempty"`
}

// Profile is what a company is ranked against. It is deliberately a plain preference
// description with no personal data in it: skills, sectors and places, not a CV. A private
// consumer keeps the CV; the engine only needs to know what to look for.
type Profile struct {
	Slug       string        `json:"slug"`
	Headline   string        `json:"headline,omitempty"`
	Countries  []string      `json:"countries,omitempty"`
	Regions    []string      `json:"regions,omitempty"`
	Sectors    []string      `json:"sectors,omitempty"`
	Languages  []string      `json:"languages,omitempty"`
	Skills     []SkillWeight `json:"skills,omitempty"`
	MustHave   []string      `json:"must_have,omitempty"`
	Exclude    []string      `json:"exclude,omitempty"`
	SizeBucket []string      `json:"size_buckets,omitempty"`
	// MinScore is the threshold below which a company is not worth reviewing.
	MinScore float64 `json:"min_score,omitempty"`
}

// Reason is one explainable contribution to a score. Ranking that cannot say why it ranked
// something is not reviewable, so every point a company scores comes with one of these.
type Reason struct {
	Code   string  `json:"code"`
	Detail string  `json:"detail"`
	Points float64 `json:"points"`
}

// Ranking is a company's score against one profile.
type Ranking struct {
	CompanyID   string    `json:"company_id"`
	ProfileSlug string    `json:"profile_slug"`
	Score       float64   `json:"score"`
	Reasons     []Reason  `json:"reasons"`
	ComputedAt  time.Time `json:"computed_at"`
}
