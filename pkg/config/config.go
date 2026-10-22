// Package config loads and validates the engine's configuration.
//
// Countries, sectors and association directories are data, not code. Adding France, adding
// the automotive sector, or adding a new member directory means writing a YAML file — the
// engine is not recompiled and no core logic changes. That is the property this package
// exists to protect, so it is strict: an unknown key is an error rather than a silently
// ignored typo, because a typo in a selector costs a whole crawl.
//
// Validation reports every problem it finds rather than stopping at the first, since
// fixing config is much faster when you can see the whole list.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// Region is an administrative area used for geographic preference. Aliases exist because a
// directory writes "Ile-de-France", "Île-de-France" and "Paris Region" for one place.
type Region struct {
	Code    string   `yaml:"code"`
	Name    string   `yaml:"name"`
	Aliases []string `yaml:"aliases,omitempty"`
}

// ContactPolicy is per-country because recruitment practice is not uniform across the EU.
// What is ordinary in one member state is unwelcome in another, so the choice is config and
// the reasoning is written down next to it.
type ContactPolicy struct {
	// PreferRecruitmentContacts puts published application addresses ahead of any other.
	PreferRecruitmentContacts bool `yaml:"prefer_recruitment_contacts"`
	// AllowGeneralFallback permits a general inbox (contact@, info@) to be surfaced as a
	// candidate at all. When false, a company with only a general inbox is reported as
	// having no usable contact instead.
	AllowGeneralFallback bool `yaml:"allow_general_fallback"`
	// RequireCareersPageCheck asks enrichment to look for an official careers page and
	// prefer it over sending anything at all.
	RequireCareersPageCheck bool `yaml:"require_careers_page_check"`
}

// Country is one country's configuration.
type Country struct {
	Code      string   `yaml:"code"`
	Name      string   `yaml:"name"`
	Languages []string `yaml:"languages"`
	Regions   []Region `yaml:"regions,omitempty"`
	// Associations names association slugs expected for this country. Advisory: it
	// documents intent and lets `config validate` point out a missing file.
	Associations  []string      `yaml:"associations,omitempty"`
	ContactPolicy ContactPolicy `yaml:"contact_policy"`
	// ComplianceNotes records what was established about recruitment and data-protection
	// practice in this country, and what is still an open question. Not legal advice and
	// not machine-read — it is there so an operator decides knowingly. See
	// docs/compliance.md.
	ComplianceNotes string `yaml:"compliance_notes,omitempty"`
}

// Sector is an industry with the vocabulary needed to recognise it in page text.
type Sector struct {
	Slug string `yaml:"slug"`
	Name string `yaml:"name"`
	// Keywords are the terms that identify the sector, in any configured language.
	Keywords []string `yaml:"keywords"`
	// Synonyms maps a canonical term to its other spellings and local-language forms.
	Synonyms map[string][]string `yaml:"synonyms,omitempty"`
	// Activities are the things companies in this sector do, used for matching
	// descriptions rather than names.
	Activities []string `yaml:"activities,omitempty"`
	// NegativeKeywords mark a company as probably *not* in this sector despite a keyword
	// hit — the cheapest way to keep a recruitment agency out of a manufacturer list.
	NegativeKeywords []string `yaml:"negative_keywords,omitempty"`
}

// AllTerms returns every term that should count as evidence for this sector, lowercased,
// accent-stripped and deduplicated.
func (s Sector) AllTerms() []string {
	seen := map[string]bool{}
	add := func(terms ...string) {
		for _, term := range terms {
			normalized := strings.ToLower(normalize.StripAccents(strings.TrimSpace(term)))
			if normalized != "" {
				seen[normalized] = true
			}
		}
	}
	add(s.Keywords...)
	add(s.Activities...)
	for canonical, aliases := range s.Synonyms {
		add(canonical)
		add(aliases...)
	}
	out := make([]string, 0, len(seen))
	for term := range seen {
		out = append(out, term)
	}
	sort.Strings(out)
	return out
}

// Limits mirrors model.CrawlLimits but with YAML-friendly types: a duration is written
// "20s" and a size "2MB" would be a surprise, so sizes are plain bytes.
type Limits struct {
	MaxPages        int     `yaml:"max_pages,omitempty"`
	MaxRecords      int     `yaml:"max_records,omitempty"`
	RequestsPerMin  float64 `yaml:"requests_per_minute,omitempty"`
	Timeout         string  `yaml:"timeout,omitempty"`
	MaxResponseSize int64   `yaml:"max_response_bytes,omitempty"`
}

// Association is one configured source of company listings.
type Association struct {
	Slug         string   `yaml:"slug"`
	Name         string   `yaml:"name"`
	Country      string   `yaml:"country"`
	Sectors      []string `yaml:"sectors,omitempty"`
	DirectoryURL string   `yaml:"directory_url,omitempty"`
	// Adapter names the registered adapter that can read this directory. See
	// pkg/directory for what is available and docs/adapter-protocol.md for adding one.
	Adapter string `yaml:"adapter"`
	// AccessMethod declares how this directory may be read. An adapter cannot quietly
	// escalate beyond what is declared here.
	AccessMethod model.AccessMethod `yaml:"access_method"`
	// Settings is adapter-specific. For the declarative HTML adapter it holds the CSS
	// selectors and pagination rules, which is what lets a new directory be added without
	// writing any code.
	Settings map[string]any    `yaml:"settings,omitempty"`
	Limits   Limits            `yaml:"limits,omitempty"`
	Notes    string            `yaml:"notes,omitempty"`
	Labels   map[string]string `yaml:"labels,omitempty"`
	// Disabled keeps a source in the repository, with its notes, while excluding it from
	// runs. Sources that turn out to be unreadable are disabled and documented, not
	// deleted: the next person needs to know it was tried and why it failed.
	Disabled bool `yaml:"disabled,omitempty"`
	// DisabledReason must be set when Disabled is true.
	DisabledReason string `yaml:"disabled_reason,omitempty"`

	sourceFile string
}

// SourceFile is the path the association was loaded from, for error messages.
func (a Association) SourceFile() string { return a.sourceFile }

// Model converts to the runtime type, resolving the duration string.
func (a Association) Model() (model.Association, error) {
	limits := model.CrawlLimits{
		MaxPages:        a.Limits.MaxPages,
		MaxRecords:      a.Limits.MaxRecords,
		RequestsPerMin:  a.Limits.RequestsPerMin,
		MaxResponseSize: a.Limits.MaxResponseSize,
	}
	if a.Limits.Timeout != "" {
		parsed, err := time.ParseDuration(a.Limits.Timeout)
		if err != nil {
			return model.Association{}, fmt.Errorf(
				"association %q: timeout %q is not a duration (try \"20s\")", a.Slug, a.Limits.Timeout)
		}
		limits.Timeout = parsed
	}
	return model.Association{
		Slug:         a.Slug,
		Name:         a.Name,
		Country:      a.Country,
		Sectors:      a.Sectors,
		DirectoryURL: a.DirectoryURL,
		Adapter:      a.Adapter,
		AccessMethod: a.AccessMethod,
		Settings:     a.Settings,
		Limits:       limits,
		Notes:        a.Notes,
		Labels:       a.Labels,
	}, nil
}

// Bundle is a whole loaded configuration.
type Bundle struct {
	Dir          string
	Countries    map[string]Country
	Sectors      map[string]Sector
	Associations map[string]Association
	Profiles     map[string]Profile
}

// CountryCodes returns the configured country codes, sorted.
func (b *Bundle) CountryCodes() []string { return sortedKeys(b.Countries) }

// SectorSlugs returns the configured sector slugs, sorted.
func (b *Bundle) SectorSlugs() []string { return sortedKeys(b.Sectors) }

// ProfileSlugs returns the configured ranking profile slugs, sorted.
func (b *Bundle) ProfileSlugs() []string { return sortedKeys(b.Profiles) }

// AssociationSlugs returns the configured association slugs, sorted.
func (b *Bundle) AssociationSlugs() []string { return sortedKeys(b.Associations) }

// EnabledAssociations returns the associations a run should visit, sorted by slug.
func (b *Bundle) EnabledAssociations() []Association {
	out := make([]Association, 0, len(b.Associations))
	for _, slug := range b.AssociationSlugs() {
		if association := b.Associations[slug]; !association.Disabled {
			out = append(out, association)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Problem is one validation failure, named by the file it is in so it can be fixed without
// hunting for it.
type Problem struct {
	File    string
	Message string
}

func (p Problem) String() string {
	if p.File == "" {
		return p.Message
	}
	return fmt.Sprintf("%s: %s", p.File, p.Message)
}

// ValidationError collects every problem found in one load.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return e.Problems[0].String()
	}
	lines := make([]string, 0, len(e.Problems)+1)
	lines = append(lines, fmt.Sprintf("%d configuration problems:", len(e.Problems)))
	for _, problem := range e.Problems {
		lines = append(lines, "  - "+problem.String())
	}
	return strings.Join(lines, "\n")
}

// DefaultDir is where the CLI looks when no directory is given.
const DefaultDir = "config"

// Load reads every country, sector and association under dir and validates them together.
//
// knownAdapters, when non-empty, is checked against each association's adapter name so that
// a directory naming an adapter nobody registered is caught by `config validate` instead of
// halfway through a crawl. Pass nil to skip that check.
func Load(dir string, knownAdapters []string) (*Bundle, error) {
	if dir == "" {
		dir = DefaultDir
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("config directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("config path %q is not a directory", dir)
	}

	bundle := &Bundle{
		Dir:          dir,
		Countries:    map[string]Country{},
		Sectors:      map[string]Sector{},
		Associations: map[string]Association{},
		Profiles:     map[string]Profile{},
	}
	var problems []Problem

	problems = append(problems, loadInto(filepath.Join(dir, "countries"), func(path string, c Country) {
		if existing, clash := bundle.Countries[c.Code]; clash {
			problems = append(problems, Problem{path,
				fmt.Sprintf("country code %q is already defined as %q", c.Code, existing.Name)})
			return
		}
		bundle.Countries[c.Code] = c
	}, &problems)...)

	problems = append(problems, loadInto(filepath.Join(dir, "sectors"), func(path string, s Sector) {
		if _, clash := bundle.Sectors[s.Slug]; clash {
			problems = append(problems, Problem{path,
				fmt.Sprintf("sector slug %q is already defined", s.Slug)})
			return
		}
		bundle.Sectors[s.Slug] = s
	}, &problems)...)

	problems = append(problems, loadInto(filepath.Join(dir, "associations"),
		func(path string, a Association) {
			a.sourceFile = path
			if _, clash := bundle.Associations[a.Slug]; clash {
				problems = append(problems, Problem{path,
					fmt.Sprintf("association slug %q is already defined", a.Slug)})
				return
			}
			bundle.Associations[a.Slug] = a
		}, &problems)...)

	// Ranking profiles are optional: a configuration with none is one that does not
	// rank, not a broken one. loadInto already treats a missing directory as nothing to do.
	problems = append(problems, loadInto(filepath.Join(dir, "profiles"),
		func(path string, p Profile) {
			p.sourceFile = path
			if _, clash := bundle.Profiles[p.Slug]; clash {
				problems = append(problems, Problem{path,
					fmt.Sprintf("profile slug %q is already defined", p.Slug)})
				return
			}
			bundle.Profiles[p.Slug] = p
		}, &problems)...)

	problems = append(problems, bundle.validate(knownAdapters)...)
	problems = append(problems, bundle.validateProfiles()...)
	if len(problems) > 0 {
		sort.Slice(problems, func(i, j int) bool {
			if problems[i].File != problems[j].File {
				return problems[i].File < problems[j].File
			}
			return problems[i].Message < problems[j].Message
		})
		return bundle, &ValidationError{Problems: problems}
	}
	return bundle, nil
}

// loadInto reads every .yaml/.yml file in one subdirectory. A missing subdirectory is not an
// error: a configuration with no sectors yet is incomplete, not malformed, and validate
// says so in better words than a stat failure would.
func loadInto[T any](dir string, accept func(path string, value T), problems *[]Problem) []Problem {
	var found []Problem
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []Problem{{dir, err.Error()}}
	}
	for _, entry := range entries {
		if entry.IsDir() || !isYAML(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		value, err := decodeFile[T](path)
		if err != nil {
			found = append(found, Problem{path, err.Error()})
			continue
		}
		accept(path, value)
	}
	return found
}

func isYAML(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".yaml") || strings.HasSuffix(lower, ".yml")
}

// decodeFile decodes one YAML document with strict field checking, so a mistyped key is
// reported rather than ignored.
func decodeFile[T any](path string) (T, error) {
	var value T
	file, err := os.Open(path)
	if err != nil {
		return value, err
	}
	// Read-only, as above: the decode error is what matters and it is returned.
	defer func() { _ = file.Close() }()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&value); err != nil {
		if errors.Is(err, io.EOF) {
			return value, errors.New("file is empty")
		}
		return value, err
	}
	// A second document in the same file would be silently dropped.
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return value, errors.New("file holds more than one YAML document; use one file each")
	}
	return value, nil
}

var slugPattern = func(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func (b *Bundle) validate(knownAdapters []string) []Problem {
	var problems []Problem
	add := func(file, format string, args ...any) {
		problems = append(problems, Problem{file, fmt.Sprintf(format, args...)})
	}

	if len(b.Countries) == 0 {
		add("", "no countries configured: add %s/countries/<code>.yaml", b.Dir)
	}

	for code, country := range b.Countries {
		file := filepath.Join(b.Dir, "countries", code+".yaml")
		if len(code) != 2 || !slugPattern(code) {
			add(file, "country code %q must be two lowercase letters (ISO 3166-1 alpha-2)", code)
		}
		if strings.TrimSpace(country.Name) == "" {
			add(file, "country %q has no name", code)
		}
		if len(country.Languages) == 0 {
			add(file, "country %q lists no languages; letters and matching need at least one", code)
		}
		regionCodes := map[string]bool{}
		for _, region := range country.Regions {
			if !slugPattern(region.Code) {
				add(file, "country %q: region code %q must be lowercase letters, digits, - or _",
					code, region.Code)
			}
			if regionCodes[region.Code] {
				add(file, "country %q: region code %q appears twice", code, region.Code)
			}
			regionCodes[region.Code] = true
		}
		for _, slug := range country.Associations {
			if _, ok := b.Associations[slug]; !ok {
				add(file, "country %q expects association %q, but no %s/associations/*.yaml defines it",
					code, slug, b.Dir)
			}
		}
	}

	for slug, sector := range b.Sectors {
		file := filepath.Join(b.Dir, "sectors", slug+".yaml")
		if !slugPattern(slug) {
			add(file, "sector slug %q must be lowercase letters, digits, - or _", slug)
		}
		if strings.TrimSpace(sector.Name) == "" {
			add(file, "sector %q has no name", slug)
		}
		if len(sector.Keywords) == 0 {
			add(file, "sector %q has no keywords, so nothing can ever match it", slug)
		}
		overlap := map[string]bool{}
		for _, term := range sector.AllTerms() {
			overlap[term] = true
		}
		for _, negative := range sector.NegativeKeywords {
			normalized := strings.ToLower(normalize.StripAccents(strings.TrimSpace(negative)))
			if overlap[normalized] {
				add(file, "sector %q lists %q as both a keyword and a negative keyword",
					slug, negative)
			}
		}
	}

	adapters := map[string]bool{}
	for _, name := range knownAdapters {
		adapters[name] = true
	}

	for slug, association := range b.Associations {
		file := association.sourceFile
		if !slugPattern(slug) {
			add(file, "association slug %q must be lowercase letters, digits, - or _", slug)
		}
		if strings.TrimSpace(association.Name) == "" {
			add(file, "association %q has no name", slug)
		}
		if _, ok := b.Countries[association.Country]; !ok {
			add(file, "association %q names country %q, which is not configured in %s/countries",
				slug, association.Country, b.Dir)
		}
		for _, sectorSlug := range association.Sectors {
			if _, ok := b.Sectors[sectorSlug]; !ok {
				add(file, "association %q names sector %q, which is not configured in %s/sectors",
					slug, sectorSlug, b.Dir)
			}
		}
		if association.Adapter == "" {
			add(file, "association %q names no adapter", slug)
		} else if len(adapters) > 0 && !adapters[association.Adapter] && !association.Disabled {
			// Only enabled sources must name a registered adapter. A disabled source is
			// often disabled *because* its adapter does not exist yet — a worked example
			// kept for the next person. Failing validation on those would push people to
			// delete the documentation instead of keeping it.
			add(file, "association %q names adapter %q; registered adapters are %s",
				slug, association.Adapter, strings.Join(sortedStrings(knownAdapters), ", "))
		}
		switch association.AccessMethod {
		case model.AccessHTML, model.AccessAPI, model.AccessDataset, model.AccessManual:
		case "":
			add(file, "association %q declares no access_method (html, api, dataset or manual)", slug)
		default:
			add(file, "association %q declares access_method %q, which is not one of "+
				"html, api, dataset, manual", slug, association.AccessMethod)
		}
		// A fetching adapter needs somewhere to start; a manual one must not have a URL
		// pretending it fetches.
		if association.AccessMethod != model.AccessManual && association.DirectoryURL == "" {
			add(file, "association %q has access_method %q but no directory_url",
				slug, association.AccessMethod)
		}
		if association.DirectoryURL != "" && normalize.URL(association.DirectoryURL) == "" {
			add(file, "association %q: directory_url %q is not an http(s) url",
				slug, association.DirectoryURL)
		}
		if association.Limits.Timeout != "" {
			if _, err := time.ParseDuration(association.Limits.Timeout); err != nil {
				add(file, "association %q: timeout %q is not a duration (try \"20s\")",
					slug, association.Limits.Timeout)
			}
		}
		if association.Limits.RequestsPerMin < 0 {
			add(file, "association %q: requests_per_minute cannot be negative", slug)
		}
		if association.Disabled && strings.TrimSpace(association.DisabledReason) == "" {
			add(file, "association %q is disabled without a disabled_reason; the next person "+
				"needs to know what was tried and why it failed", slug)
		}
	}
	return problems
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
