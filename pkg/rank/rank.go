// Package rank scores a company against a profile, deterministically and offline.
//
// Three rules shape everything here.
//
// It is pure. This package imports pkg/model, pkg/config and pkg/normalize, and nothing
// else — never the store, the fetcher, the extractor or the directory adapters. Ranking
// cannot touch contact extraction or anything that sends, because it cannot see them.
//
// The clock is a parameter. Freshness is one of the factors, so a scorer calling
// time.Now() internally could not be tested against a golden file and would score the same
// company differently on two consecutive days for no reason the operator asked for.
//
// Reasons are an audit, not a highlight reel. Every sub-factor emits a reason even when it
// contributes nothing, and the points always sum to the score. A score with no explanation
// is not reviewable, so that equality is the contract this package exists to keep.
package rank

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/config"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

// Reason codes. These are stable identifiers: once published they are part of the contract,
// with the same discipline as exit codes. Anything consuming a ranking may switch on them.
const (
	ReasonBasis   = "score.basis"
	ReasonClamped = "score.clamped"

	ReasonSkillMatch   = "skill.match"
	ReasonSkillPenalty = "skill.penalty"
	ReasonSkillCapped  = "skill.capped"
	ReasonSkillNone    = "skill.none"

	ReasonSectorMatch    = "sector.match"
	ReasonSectorDeclared = "sector.declared"
	ReasonSectorNegative = "sector.negative"
	ReasonSectorCapped   = "sector.capped"
	ReasonSectorNone     = "sector.none"

	ReasonGeoCountry      = "geo.country"
	ReasonGeoCountryOther = "geo.country.other"
	ReasonGeoRegion       = "geo.region"
	ReasonGeoRegionOther  = "geo.region.other"
	ReasonGeoRegionNone   = "geo.region.unknown"

	ReasonContactRecruitment = "contact.recruitment"
	ReasonContactHR          = "contact.hr"
	ReasonContactGeneral     = "contact.general"
	ReasonContactOther       = "contact.other"
	ReasonContactCareers     = "contact.careers_page"
	ReasonContactPolicy      = "contact.policy"
	ReasonContactNone        = "contact.none"

	ReasonEvidenceQuality   = "evidence.quality"
	ReasonEvidenceFreshness = "evidence.freshness"
	ReasonEvidenceUnchecked = "evidence.unchecked"
	ReasonEvidenceFailed    = "evidence.crawl_failed"

	ReasonLanguageMatch   = "language.match"
	ReasonLanguageAssumed = "language.assumed"
	ReasonLanguageOther   = "language.other"

	ReasonSizeMatch    = "size.match"
	ReasonSizeUnknown  = "size.unknown"
	ReasonSizeMismatch = "size.mismatch"

	ReasonExcludeVeto     = "exclude.veto"
	ReasonMustHaveMissing = "must_have.missing"
)

// Options configures a Ranker.
type Options struct {
	// Bundle supplies the sector vocabulary, each country's regions and languages, and
	// the per-country contact policy. Required.
	Bundle *config.Bundle
	// Weights is the sub-factor table. The zero value means DefaultWeights.
	Weights Weights
	// Now is the reference instant for freshness. Required: see the package comment.
	Now time.Time
}

// compiledSkill is one profile skill with its search terms pre-normalised.
type compiledSkill struct {
	label  string
	weight float64
	terms  []string // the term itself, then its aliases, in file order
}

// compiledSector is one sector's vocabulary, resolved once.
type compiledSector struct {
	slug     string
	terms    []string
	negative []string
}

// Ranker scores companies against one profile. Build it with New and reuse it: it holds
// the profile's terms and the sectors' vocabularies already normalised, which matters
// because config.Sector.AllTerms allocates and sorts on every call.
type Ranker struct {
	profile   model.Profile
	weights   Weights
	now       time.Time
	bundle    *config.Bundle
	scale     float64
	basis     float64
	active    []string
	skills    []compiledSkill
	posTotal  float64
	sectors   []compiledSector
	mustHave  []string
	exclude   []string
	regions   map[string][]string // region code -> code, name and aliases, normalised
	langs     map[string]bool
	sizes     map[string]bool
	countries map[string]bool
}

// New compiles a profile into a Ranker.
//
// It re-validates the profile even though config.Load already did, because this is a
// library a private consumer may call with a hand-built model.Profile. The same two-layer
// discipline the engine applies to personal addresses: a rule worth having is worth
// enforcing where it can be reached.
func New(profile model.Profile, opts Options) (*Ranker, error) {
	if opts.Bundle == nil {
		return nil, fmt.Errorf("profile %q: no configuration bundle given", profile.Slug)
	}
	if opts.Now.IsZero() {
		return nil, fmt.Errorf("profile %q: Options.Now is required, so that freshness "+
			"scoring is reproducible", profile.Slug)
	}
	weights := opts.Weights
	if weights == (Weights{}) {
		weights = DefaultWeights()
	}
	if err := weights.Validate(); err != nil {
		return nil, fmt.Errorf("profile %q: %w", profile.Slug, err)
	}
	if len(profile.Skills) == 0 && len(profile.Sectors) == 0 && len(profile.Countries) == 0 {
		return nil, fmt.Errorf("profile %q expresses no preference, so every company "+
			"would score the same", profile.Slug)
	}

	r := &Ranker{
		profile: profile, weights: weights, now: opts.Now.UTC(), bundle: opts.Bundle,
		regions: map[string][]string{}, langs: map[string]bool{},
		sizes: map[string]bool{}, countries: map[string]bool{},
	}

	for _, skill := range profile.Skills {
		term := normalizeTerm(skill.Term)
		if term == "" {
			return nil, fmt.Errorf("profile %q has a skill with an empty term", profile.Slug)
		}
		terms := []string{term}
		for _, alias := range skill.Aliases {
			if normalized := normalizeTerm(alias); normalized != "" && normalized != term {
				terms = append(terms, normalized)
			}
		}
		r.skills = append(r.skills, compiledSkill{
			label: skill.Term, weight: skill.Weight, terms: terms,
		})
		if skill.Weight > 0 {
			r.posTotal += skill.Weight
		}
	}
	if len(r.skills) > 0 && r.posTotal <= 0 {
		return nil, fmt.Errorf("profile %q has skills but no positive weight among them, "+
			"so the skills score has nothing to divide by", profile.Slug)
	}

	for _, slug := range profile.Sectors {
		sector, ok := opts.Bundle.Sectors[slug]
		if !ok {
			return nil, fmt.Errorf("profile %q names sector %q, which is not configured",
				profile.Slug, slug)
		}
		compiled := compiledSector{slug: slug, terms: sector.AllTerms()}
		for _, negative := range sector.NegativeKeywords {
			if normalized := normalizeTerm(negative); normalized != "" {
				compiled.negative = append(compiled.negative, normalized)
			}
		}
		r.sectors = append(r.sectors, compiled)
	}

	for _, code := range profile.Countries {
		code = strings.ToLower(strings.TrimSpace(code))
		r.countries[code] = true
		country, ok := opts.Bundle.Countries[code]
		if !ok {
			return nil, fmt.Errorf("profile %q names country %q, which is not configured",
				profile.Slug, code)
		}
		// A region is matched by its code, its name or any alias, because a directory
		// writes "Bayern", "Bavaria" or "Munich" for the same place.
		for _, region := range country.Regions {
			spellings := []string{normalizeTerm(region.Code), normalizeTerm(region.Name)}
			for _, alias := range region.Aliases {
				spellings = append(spellings, normalizeTerm(alias))
			}
			r.regions[region.Code] = spellings
		}
	}
	for _, language := range profile.Languages {
		r.langs[strings.ToLower(strings.TrimSpace(language))] = true
	}
	for _, bucket := range profile.SizeBucket {
		r.sizes[strings.ToLower(strings.TrimSpace(bucket))] = true
	}
	for _, term := range profile.MustHave {
		if normalized := normalizeTerm(term); normalized != "" {
			r.mustHave = append(r.mustHave, normalized)
		}
	}
	for _, term := range profile.Exclude {
		if normalized := normalizeTerm(term); normalized != "" {
			r.exclude = append(r.exclude, normalized)
		}
	}

	r.computeBasis()
	return r, nil
}

// computeBasis works out which sub-factors this profile expresses and the scale that brings
// them back to 100.
//
// Without rescaling, a profile that states no language preference could never exceed 95, so
// min_score would silently mean something different in every file. Rescaling is what makes
// a score comparable across profiles, which is the whole reason the scale is bounded.
func (r *Ranker) computeBasis() {
	add := func(name string, weight float64, expressed bool) {
		if expressed && weight > 0 {
			r.basis += weight
			r.active = append(r.active, name)
		}
	}
	add("skills", r.weights.Skills, len(r.skills) > 0)
	add("sector", r.weights.Sector, len(r.sectors) > 0)
	add("country", r.weights.GeoCountry, len(r.countries) > 0)
	add("region", r.weights.GeoRegion, len(r.regions) > 0)
	// Contact and evidence are properties of the record rather than of the profile: a
	// company nobody can reach, or that nothing evidences, is worth less to every profile.
	add("contact", r.weights.Contact, true)
	add("evidence", r.weights.EvidenceQuality+r.weights.EvidenceFreshness, true)
	add("language", r.weights.Language, len(r.langs) > 0)
	add("size", r.weights.Size, len(r.sizes) > 0)

	if r.basis > 0 {
		r.scale = 100 / r.basis
	}
}

// Profile returns the profile this ranker scores against.
func (r *Ranker) Profile() model.Profile { return r.profile }

// Basis is the total sub-factor weight this profile expresses, before rescaling.
func (r *Ranker) Basis() float64 { return r.basis }

// Active names the sub-factors this profile expresses.
func (r *Ranker) Active() []string { return append([]string(nil), r.active...) }

// scorer accumulates points and reasons for one company.
type scorer struct {
	r       *Ranker
	reasons []model.Reason
	total   float64
}

// award converts a sub-factor's value in [-1, 1] into points and records why.
func (s *scorer) award(code string, value, weight float64, detail string) {
	points := value * weight * s.r.scale
	s.total += points
	s.reasons = append(s.reasons, model.Reason{Code: code, Detail: detail, Points: round(points)})
}

// note records something worth saying that carries no points.
func (s *scorer) note(code, detail string) {
	s.reasons = append(s.reasons, model.Reason{Code: code, Detail: detail, Points: 0})
}

// Score ranks one company. It never fails: every company is scorable.
func (r *Ranker) Score(c model.Company) model.Ranking {
	doc := NewDocument(c)

	// Exclude vetoes before anything else is computed. A vetoed company carries exactly
	// one reason, and the sum invariant holds trivially at zero.
	if term, where, found := doc.FindAny(r.exclude); found {
		return r.ranking(c, 0, []model.Reason{{
			Code:   ReasonExcludeVeto,
			Detail: fmt.Sprintf("%q in %s", term, where),
			Points: 0,
		}})
	}
	// MustHave gates, naming every missing term rather than the first — the same
	// multi-problem courtesy config validation extends.
	var missing []string
	for _, term := range r.mustHave {
		if _, found := doc.Find(term); !found {
			missing = append(missing, term)
		}
	}
	if len(missing) > 0 {
		return r.ranking(c, 0, []model.Reason{{
			Code:   ReasonMustHaveMissing,
			Detail: fmt.Sprintf("says nothing about %s", quoteList(missing)),
			Points: 0,
		}})
	}

	s := &scorer{r: r}
	s.note(ReasonBasis, fmt.Sprintf("scored over %s; %.1f points available",
		strings.Join(r.active, ", "), 100.0))

	r.scoreSkills(s, doc)
	r.scoreSector(s, doc, c)
	r.scoreGeography(s, c)
	r.scoreContact(s, c)
	r.scoreEvidence(s, c)
	r.scoreLanguage(s, c)
	r.scoreSize(s, c)

	// Clamp last, and say so, so that the points still sum to the score.
	final := s.total
	if final < 0 || final > 100 {
		clamped := math.Min(100, math.Max(0, final))
		s.reasons = append(s.reasons, model.Reason{
			Code:   ReasonClamped,
			Detail: fmt.Sprintf("raw score %.1f brought inside 0-100", final),
			Points: round(clamped - final),
		})
		final = clamped
	}
	return r.ranking(c, round(final), s.reasons)
}

func (r *Ranker) ranking(c model.Company, score float64, reasons []model.Reason) model.Ranking {
	return model.Ranking{
		CompanyID: c.ID, ProfileSlug: r.profile.Slug,
		Score: score, Reasons: reasons, ComputedAt: r.now,
	}
}

func (r *Ranker) scoreSkills(s *scorer, doc Document) {
	if len(r.skills) == 0 {
		return
	}
	matched := 0.0
	for _, skill := range r.skills {
		term, where, found := doc.FindAny(skill.terms)
		if !found {
			continue
		}
		matched += skill.weight
		share := skill.weight / r.posTotal
		code := ReasonSkillMatch
		if skill.weight < 0 {
			code = ReasonSkillPenalty
		}
		// Name the spelling that actually matched when it was an alias: knowing a skill
		// was found as "drug safety" rather than "pharmakovigilanz" tells the reader
		// something about the company, not just about the profile.
		detail := fmt.Sprintf("%q (weight %.1f) in %s", skill.label, skill.weight, where)
		if term != normalizeTerm(skill.label) {
			detail = fmt.Sprintf("%q as %q (weight %.1f) in %s",
				skill.label, term, skill.weight, where)
		}
		s.award(code, share, r.weights.Skills, detail)
	}
	if matched == 0 {
		s.note(ReasonSkillNone, "none of the profile's skill terms appear")
		return
	}
	// The share can exceed the range when negative weights pile up or when the matched
	// positive weight is the whole total; correct it visibly rather than silently.
	value := matched / r.posTotal
	if value > 1 || value < -1 {
		corrected := math.Min(1, math.Max(-1, value))
		s.award(ReasonSkillCapped, corrected-value, r.weights.Skills,
			fmt.Sprintf("skill share %.2f brought inside -1..1", value))
	}
}

func (r *Ranker) scoreSector(s *scorer, doc Document, c model.Company) {
	if len(r.sectors) == 0 {
		return
	}
	// A company is relevant to *a* sector, not to the sum of several: take the best, or
	// two overlapping vocabularies would double-count the same words.
	best, bestValue, bestHits, bestNegative := compiledSector{}, math.Inf(-1), 0, 0
	for _, sector := range r.sectors {
		hits := doc.CountDistinct(sector.terms)
		negatives := doc.CountDistinct(sector.negative)
		// Saturating at three: three independent vocabulary hits is as much as a keyword
		// list can prove, and it stops a keyword-stuffed description running away.
		value := math.Min(float64(hits), 3) / 3
		value -= 0.4 * float64(negatives)
		if value > bestValue {
			best, bestValue, bestHits, bestNegative = sector, value, hits, negatives
		}
	}

	declaredWeight := 0.0
	for _, declared := range c.Sectors {
		if declared == best.slug {
			declaredWeight = r.weights.SectorDeclaredMax
			break
		}
	}
	// The vocabulary is worth what is left after the directory's own filing.
	vocabularyWeight := r.weights.Sector - r.weights.SectorDeclaredMax

	if bestHits > 0 {
		s.award(ReasonSectorMatch, math.Min(float64(bestHits), 3)/3, vocabularyWeight,
			fmt.Sprintf("%s: %d vocabulary term(s) in the company's own text", best.slug, bestHits))
	} else {
		s.note(ReasonSectorNone, fmt.Sprintf("%s: no vocabulary term appears", best.slug))
	}
	if bestNegative > 0 {
		s.award(ReasonSectorNegative, -0.4*float64(bestNegative), vocabularyWeight,
			fmt.Sprintf("%s: %d negative keyword(s) suggest this is not such a company",
				best.slug, bestNegative))
	}
	if declaredWeight > 0 {
		s.award(ReasonSectorDeclared, 1, declaredWeight,
			fmt.Sprintf("listed under sector %q", best.slug))
	}
}

func (r *Ranker) scoreGeography(s *scorer, c model.Company) {
	if len(r.countries) > 0 {
		if r.countries[strings.ToLower(c.Country)] {
			s.award(ReasonGeoCountry, 1, r.weights.GeoCountry,
				fmt.Sprintf("%s is a preferred country", c.Country))
		} else {
			s.note(ReasonGeoCountryOther,
				fmt.Sprintf("%s is not among the preferred countries", c.Country))
		}
	}
	if len(r.regions) == 0 || len(r.profile.Regions) == 0 {
		return
	}
	region := normalizeTerm(c.Region)
	if region == "" {
		s.note(ReasonGeoRegionNone, "no region recorded for this company")
		return
	}
	for _, preferred := range r.profile.Regions {
		for _, spelling := range r.regions[preferred] {
			if spelling != "" && spelling == region {
				s.award(ReasonGeoRegion, 1, r.weights.GeoRegion,
					fmt.Sprintf("%s matches preferred region %q", c.Region, preferred))
				return
			}
		}
	}
	s.note(ReasonGeoRegionOther,
		fmt.Sprintf("%s is not among the preferred regions", c.Region))
}

func (r *Ranker) scoreContact(s *scorer, c model.Company) {
	best, hasContact := c.BestContact()
	if !hasContact {
		s.note(ReasonContactNone, "no contact the engine was willing to record")
		if c.CareersURL != "" {
			s.award(ReasonContactCareers, 0.8, r.weights.Contact,
				"a careers page is published, which is the better channel anyway")
		}
		return
	}

	// A general inbox is a fallback, not an invitation — and whether it may be surfaced at
	// all is a judgement the country's config already records.
	if best.Kind == model.ContactGeneral {
		if country, ok := r.bundle.Countries[strings.ToLower(c.Country)]; ok &&
			!country.ContactPolicy.AllowGeneralFallback {
			s.note(ReasonContactPolicy, fmt.Sprintf(
				"only a general inbox, and %s's contact policy does not allow that as a fallback",
				c.Country))
			return
		}
	}

	value, code := 0.2, ReasonContactOther
	switch best.Kind {
	case model.ContactRecruitment:
		value, code = 1.0, ReasonContactRecruitment
	case model.ContactHR:
		value, code = 0.7, ReasonContactHR
	case model.ContactGeneral:
		value, code = 0.3, ReasonContactGeneral
	}
	detail := fmt.Sprintf("published %s address", best.Kind)
	if best.Verification != model.VerificationPublished {
		// An address nobody saw published is worth half of one that was.
		value /= 2
		detail = fmt.Sprintf("%s address, but %s rather than published", best.Kind, best.Verification)
	}
	// A careers page is at least as good a channel as an HR inbox.
	if c.CareersURL != "" && value < 0.8 {
		value = 0.8
		detail += "; a careers page is published, which is the better channel"
	}
	s.award(code, value, r.weights.Contact, detail)
}

func (r *Ranker) scoreEvidence(s *scorer, c model.Company) {
	quality := 0.2
	switch c.Quality {
	case model.QualityHigh:
		quality = 1.0
	case model.QualityMedium:
		quality = 0.6
	}
	s.award(ReasonEvidenceQuality, quality, r.weights.EvidenceQuality,
		fmt.Sprintf("data quality is %s", orUnknown(string(c.Quality))))

	switch c.CrawlStatus {
	case model.CrawlBlocked, model.CrawlRefused, model.CrawlError:
		// A site we could not read has not been evidenced, whenever we last tried.
		s.note(ReasonEvidenceFailed,
			fmt.Sprintf("the last visit ended %s, so nothing here is freshly evidenced",
				c.CrawlStatus))
		return
	}
	if c.LastCheckedAt == nil || c.LastCheckedAt.IsZero() {
		s.note(ReasonEvidenceUnchecked, "the company's own site has never been visited")
		return
	}

	days := int(r.now.Sub(*c.LastCheckedAt).Hours() / 24)
	// Steps rather than a curve: "checked 41 days ago, within 90 days" is explainable in
	// one sentence, and a company's score does not drift between two runs on the same day.
	value, window := 0.1, "over 180 days ago"
	switch {
	case days <= 30:
		value, window = 1.0, "within 30 days"
	case days <= 90:
		value, window = 0.6, "within 90 days"
	case days <= 180:
		value, window = 0.3, "within 180 days"
	}
	s.award(ReasonEvidenceFreshness, value, r.weights.EvidenceFreshness,
		fmt.Sprintf("last checked %d day(s) ago, %s", days, window))
}

func (r *Ranker) scoreLanguage(s *scorer, c model.Company) {
	if len(r.langs) == 0 {
		return
	}
	for _, language := range c.Languages {
		if r.langs[strings.ToLower(language)] {
			s.award(ReasonLanguageMatch, 1, r.weights.Language,
				fmt.Sprintf("%s is a preferred language", language))
			return
		}
	}
	// Nothing populates Company.Languages during discovery, so without this fallback the
	// factor would be dead weight on every crawled record. An assumption from the country
	// is weaker evidence than a fact about the company, hence half credit.
	if len(c.Languages) == 0 {
		if country, ok := r.bundle.Countries[strings.ToLower(c.Country)]; ok {
			for _, language := range country.Languages {
				if r.langs[strings.ToLower(language)] {
					s.award(ReasonLanguageAssumed, 0.5, r.weights.Language,
						fmt.Sprintf("no language recorded; %s is spoken in %s",
							language, c.Country))
					return
				}
			}
		}
	}
	s.note(ReasonLanguageOther, "no preferred language is recorded or implied")
}

func (r *Ranker) scoreSize(s *scorer, c model.Company) {
	if len(r.sizes) == 0 {
		return
	}
	bucket := strings.ToLower(strings.TrimSpace(c.SizeBucket))
	switch {
	case bucket == "":
		// Nothing in the engine learns a company's size yet. Scoring unknown as a
		// mismatch would depress every company for a gap that is ours, not theirs.
		s.award(ReasonSizeUnknown, 0.5, r.weights.Size, "company size is not known")
	case r.sizes[bucket]:
		s.award(ReasonSizeMatch, 1, r.weights.Size, fmt.Sprintf("size %q is preferred", bucket))
	default:
		s.note(ReasonSizeMismatch, fmt.Sprintf("size %q is not preferred", bucket))
	}
}

// ScoreAll ranks many companies, ordered by score descending then company id, so that the
// same input always produces byte-identical output.
func (r *Ranker) ScoreAll(companies []model.Company) []model.Ranking {
	rankings := make([]model.Ranking, 0, len(companies))
	for _, c := range companies {
		rankings = append(rankings, r.Score(c))
	}
	sort.SliceStable(rankings, func(i, j int) bool {
		if rankings[i].Score != rankings[j].Score {
			return rankings[i].Score > rankings[j].Score
		}
		return rankings[i].CompanyID < rankings[j].CompanyID
	})
	return rankings
}

// Significant drops the reasons that carry no points, for display. Everything is still
// stored: answering "why is this only 40?" needs the factors that scored nothing.
func Significant(reasons []model.Reason) []model.Reason {
	out := make([]model.Reason, 0, len(reasons))
	for _, reason := range reasons {
		if reason.Points != 0 {
			out = append(out, reason)
		}
	}
	return out
}

// TopReason returns the highest-scoring reason, for a one-line summary.
func TopReason(reasons []model.Reason) (model.Reason, bool) {
	best, found := model.Reason{}, false
	for _, reason := range reasons {
		if !found || reason.Points > best.Points {
			best, found = reason, true
		}
	}
	return best, found
}

// round keeps points to one decimal, so that the stored sum and the printed sum agree.
func round(value float64) float64 { return math.Round(value*10) / 10 }

func orUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func quoteList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, fmt.Sprintf("%q", item))
	}
	return strings.Join(quoted, ", ")
}
