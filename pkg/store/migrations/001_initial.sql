-- Initial schema for the discovery engine.
--
-- Scope: companies, where they were found, and what was published about them. Application
-- drafts, sent applications and suppression lists are deliberately absent — they are
-- personal data belonging to a private consumer, which registers its own migration
-- namespace against the same database. See docs/data-contract.md.
--
-- SQL is kept to the portable subset so this can move to PostgreSQL later: TEXT timestamps
-- in RFC 3339, no SQLite-only expressions in constraints, JSON kept in TEXT columns that
-- the application encodes and decodes.

CREATE TABLE associations (
    slug          TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    country       TEXT NOT NULL,
    directory_url TEXT NOT NULL DEFAULT '',
    adapter       TEXT NOT NULL,
    access_method TEXT NOT NULL,
    sectors_json  TEXT NOT NULL DEFAULT '[]',
    settings_json TEXT NOT NULL DEFAULT '{}',
    notes         TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE companies (
    company_id       TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    normalized_name  TEXT NOT NULL,
    country          TEXT NOT NULL,
    region           TEXT NOT NULL DEFAULT '',
    website          TEXT NOT NULL DEFAULT '',
    canonical_domain TEXT NOT NULL DEFAULT '',
    description      TEXT NOT NULL DEFAULT '',
    size_bucket      TEXT NOT NULL DEFAULT '',
    languages_json   TEXT NOT NULL DEFAULT '[]',
    sectors_json     TEXT NOT NULL DEFAULT '[]',
    keywords_json    TEXT NOT NULL DEFAULT '[]',
    careers_url      TEXT NOT NULL DEFAULT '',
    data_quality     TEXT NOT NULL DEFAULT 'low',
    crawl_status     TEXT NOT NULL DEFAULT 'not_attempted',
    crawl_error      TEXT NOT NULL DEFAULT '',
    first_seen_at    TEXT NOT NULL,
    last_checked_at  TEXT,
    updated_at       TEXT NOT NULL
);

CREATE INDEX idx_companies_country ON companies (country);
CREATE INDEX idx_companies_domain ON companies (canonical_domain);
CREATE INDEX idx_companies_crawl_status ON companies (crawl_status);

-- The join table that makes discovery idempotent.
--
-- A company keeps the id of the identity it was FIRST seen under. When a later run learns
-- its website, a 'domain' identity is added here pointing at that same company_id instead
-- of a second company being created. The PRIMARY KEY is what enforces it: two runs that
-- derive the same identity cannot produce two companies.
--
-- kind is 'domain' (registrable domain — strong) or 'name' (normalised name within one
-- country — weaker, and never used to join across countries).
CREATE TABLE company_identities (
    kind       TEXT NOT NULL,
    value      TEXT NOT NULL,
    company_id TEXT NOT NULL REFERENCES companies (company_id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (kind, value)
);

CREATE INDEX idx_company_identities_company ON company_identities (company_id);

-- Provenance: every company knows which directory listed it, and when. A company may
-- appear in many associations and in many countries without becoming many companies.
CREATE TABLE company_sources (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    company_id        TEXT NOT NULL REFERENCES companies (company_id) ON DELETE CASCADE,
    association_slug  TEXT NOT NULL,
    source_url        TEXT NOT NULL,
    raw_name          TEXT NOT NULL DEFAULT '',
    retrieved_at      TEXT NOT NULL,
    UNIQUE (company_id, association_slug, source_url)
);

CREATE INDEX idx_company_sources_association ON company_sources (association_slug);

-- Published contacts. UNIQUE(company_id, address) is what stops a company with four
-- addresses turning into four applications.
CREATE TABLE contacts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    company_id    TEXT NOT NULL REFERENCES companies (company_id) ON DELETE CASCADE,
    address       TEXT NOT NULL,
    kind          TEXT NOT NULL,
    verification  TEXT NOT NULL,
    found_on_url  TEXT NOT NULL DEFAULT '',
    found_at      TEXT NOT NULL,
    notes         TEXT NOT NULL DEFAULT '',
    UNIQUE (company_id, address)
);

CREATE INDEX idx_contacts_company ON contacts (company_id);
CREATE INDEX idx_contacts_kind ON contacts (kind);

-- Failures are kept, not dropped. A source that blocks automated access has to stay
-- visible so it can be reported rather than retried forever.
CREATE TABLE crawl_runs (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    kind             TEXT NOT NULL,
    association_slug TEXT NOT NULL DEFAULT '',
    started_at       TEXT NOT NULL,
    finished_at      TEXT,
    status           TEXT NOT NULL,
    stats_json       TEXT NOT NULL DEFAULT '{}',
    error            TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_crawl_runs_started ON crawl_runs (started_at);

-- A ranking profile: skills, sectors and places. No CV, no personal data — the engine only
-- needs to know what to look for.
CREATE TABLE professional_profiles (
    slug         TEXT PRIMARY KEY,
    payload_json TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

-- Scores with their reasons. A score with no explanation is not reviewable, so reasons_json
-- is NOT NULL and the ranker always fills it.
CREATE TABLE company_rankings (
    company_id   TEXT NOT NULL REFERENCES companies (company_id) ON DELETE CASCADE,
    profile_slug TEXT NOT NULL REFERENCES professional_profiles (slug) ON DELETE CASCADE,
    score        REAL NOT NULL,
    reasons_json TEXT NOT NULL,
    computed_at  TEXT NOT NULL,
    PRIMARY KEY (company_id, profile_slug)
);

CREATE INDEX idx_company_rankings_score ON company_rankings (profile_slug, score DESC);
