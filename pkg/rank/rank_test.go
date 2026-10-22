package rank

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/config"
	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

var reference = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// testBundle builds configuration by hand, so these tests do not depend on what happens to
// be committed under config/.
func testBundle() *config.Bundle {
	return &config.Bundle{
		Dir: "testdata",
		Countries: map[string]config.Country{
			"de": {
				Code: "de", Name: "Germany", Languages: []string{"de", "en"},
				Regions: []config.Region{
					{Code: "by", Name: "Bayern", Aliases: []string{"Bavaria", "München", "Munich"}},
					{Code: "nw", Name: "Nordrhein-Westfalen", Aliases: []string{"NRW", "Köln"}},
				},
				ContactPolicy: config.ContactPolicy{
					PreferRecruitmentContacts: true, AllowGeneralFallback: true,
				},
			},
			"fr": {
				Code: "fr", Name: "France", Languages: []string{"fr"},
				ContactPolicy: config.ContactPolicy{AllowGeneralFallback: false},
			},
		},
		Sectors: map[string]config.Sector{
			"pharma": {
				Slug: "pharma", Name: "Pharma",
				Keywords:         []string{"pharmaceutical", "arzneimittel", "biotech"},
				Activities:       []string{"batch release", "sterile manufacturing"},
				NegativeKeywords: []string{"personalvermittlung", "fachzeitschrift"},
			},
		},
		Associations: map[string]config.Association{},
		Profiles:     map[string]config.Profile{},
	}
}

func testProfile() model.Profile {
	return model.Profile{
		Slug:      "pharma-qa",
		Headline:  "QA roles in pharma",
		Countries: []string{"de"},
		Regions:   []string{"by"},
		Sectors:   []string{"pharma"},
		Languages: []string{"de", "en"},
		MustHave:  nil,
		Exclude:   []string{"personalvermittlung"},
		Skills: []model.SkillWeight{
			{Term: "pharmakovigilanz", Weight: 10, Aliases: []string{"drug safety", "PV"}},
			{Term: "GMP", Weight: 5},
			{Term: "lohnabfuellung", Weight: -3},
		},
		MinScore: 40,
	}
}

func newTestRanker(t *testing.T, profile model.Profile) *Ranker {
	t.Helper()
	r, err := New(profile, Options{Bundle: testBundle(), Now: reference})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func strong() model.Company {
	checked := reference.Add(-5 * 24 * time.Hour)
	return model.Company{
		ID: "c1", Name: "Beispiel Pharma GmbH", Country: "de", Region: "Bayern",
		Description: "Arzneimittel manufacturer. Pharmakovigilanz and GMP batch release.",
		Sectors:     []string{"pharma"}, Languages: []string{"de"},
		CareersURL: "https://beispiel.example.org/karriere",
		Quality:    model.QualityHigh, CrawlStatus: model.CrawlOK, LastCheckedAt: &checked,
		Contacts: []model.Contact{{
			Address: "karriere@beispiel.example.org", Kind: model.ContactRecruitment,
			Verification: model.VerificationPublished,
			FoundOnURL:   "https://beispiel.example.org/karriere",
		}},
	}
}

// The contract this package exists to keep: the points always add up to the score.
func TestReasonsSumToScore(t *testing.T) {
	r := newTestRanker(t, testProfile())
	checked := reference.Add(-200 * 24 * time.Hour)

	companies := []model.Company{
		strong(),
		{ID: "c2", Name: "Leer GmbH", Country: "de"},
		{ID: "c3", Name: "Teil GmbH", Country: "fr", Description: "biotech",
			Quality: model.QualityMedium, CrawlStatus: model.CrawlOK, LastCheckedAt: &checked},
		{ID: "c4", Name: "Negativ GmbH", Country: "de",
			Description: "Fachzeitschrift for arzneimittel professionals",
			Sectors:     []string{"pharma"}},
		{ID: "c5", Name: "Lohn GmbH", Country: "de",
			Description: "Lohnabfuellung and GMP services", Sectors: []string{"pharma"}},
		{ID: "c6", Name: "Blockiert GmbH", Country: "de", Description: "biotech",
			CrawlStatus: model.CrawlBlocked},
	}

	for _, c := range companies {
		ranking := r.Score(c)
		sum := 0.0
		for _, reason := range ranking.Reasons {
			sum += reason.Points
		}
		if math.Abs(sum-ranking.Score) > 0.05 {
			t.Errorf("%s: reasons sum to %.2f but score is %.2f\n  %s",
				c.Name, sum, ranking.Score, formatReasons(ranking.Reasons))
		}
		if len(ranking.Reasons) == 0 {
			t.Errorf("%s: scored with no reasons at all", c.Name)
		}
	}
}

func formatReasons(reasons []model.Reason) string {
	lines := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		lines = append(lines, reason.Code+" "+reason.Detail)
	}
	return strings.Join(lines, "\n  ")
}

func TestScoreIsDeterministic(t *testing.T) {
	r := newTestRanker(t, testProfile())
	first, err := json.Marshal(r.Score(strong()))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		again, err := json.Marshal(r.Score(strong()))
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("scoring differed on run %d", i+2)
		}
	}
}

// Only freshness may depend on the clock.
func TestOnlyFreshnessDependsOnTheClock(t *testing.T) {
	bundle := testBundle()
	early, err := New(testProfile(), Options{Bundle: bundle, Now: reference})
	if err != nil {
		t.Fatal(err)
	}
	late, err := New(testProfile(), Options{Bundle: bundle, Now: reference.Add(120 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	a, b := early.Score(strong()), late.Score(strong())
	if a.Score == b.Score {
		t.Fatal("a four-month gap should change the freshness score")
	}
	// Compare whole reasons, not just codes: skill.match fires once per matched skill, so
	// a map keyed on the code alone would collapse them and report a false difference.
	tally := func(reasons []model.Reason) map[model.Reason]int {
		counts := map[model.Reason]int{}
		for _, reason := range reasons {
			if reason.Code == ReasonEvidenceFreshness {
				continue
			}
			counts[reason]++
		}
		return counts
	}
	before, after := tally(a.Reasons), tally(b.Reasons)
	for reason, count := range before {
		if after[reason] != count {
			t.Errorf("the clock changed a factor other than freshness: %+v (%d -> %d)",
				reason, count, after[reason])
		}
	}
	for reason, count := range after {
		if before[reason] != count {
			t.Errorf("the clock introduced a factor other than freshness: %+v (%d)",
				reason, count)
		}
	}
}

func TestExcludeVetoes(t *testing.T) {
	r := newTestRanker(t, testProfile())
	// A company that would otherwise score very well.
	c := strong()
	c.Description += " Personalvermittlung for the pharma industry."

	ranking := r.Score(c)
	if ranking.Score != 0 {
		t.Errorf("score = %.1f, want 0", ranking.Score)
	}
	if len(ranking.Reasons) != 1 {
		t.Fatalf("want exactly one reason, got %d: %s",
			len(ranking.Reasons), formatReasons(ranking.Reasons))
	}
	if ranking.Reasons[0].Code != ReasonExcludeVeto {
		t.Errorf("code = %q, want %q", ranking.Reasons[0].Code, ReasonExcludeVeto)
	}
	if !strings.Contains(ranking.Reasons[0].Detail, "personalvermittlung") {
		t.Errorf("the reason should name the term, got %q", ranking.Reasons[0].Detail)
	}
}

func TestMustHaveNamesEveryMissingTerm(t *testing.T) {
	profile := testProfile()
	profile.MustHave = []string{"pharma", "sterile", "biotech"}
	r := newTestRanker(t, profile)

	ranking := r.Score(model.Company{
		ID: "c1", Name: "Nur Pharma GmbH", Country: "de", Description: "pharma only",
	})
	if ranking.Score != 0 {
		t.Errorf("score = %.1f, want 0", ranking.Score)
	}
	detail := ranking.Reasons[0].Detail
	for _, missing := range []string{"sterile", "biotech"} {
		if !strings.Contains(detail, missing) {
			t.Errorf("detail should name %q, got %q", missing, detail)
		}
	}
	if strings.Contains(detail, "\"pharma\"") {
		t.Errorf("detail should not name the term that was present, got %q", detail)
	}
}

// Requirement: a company must not score merely because an association listed it.
func TestAssociationMembershipScoresNothing(t *testing.T) {
	r := newTestRanker(t, testProfile())

	bare := strong()
	bare.Sources = nil
	listed := strong()
	listed.Sources = []model.Source{
		{AssociationSlug: "a", SourceURL: "https://a.example.org", RetrievedAt: reference},
		{AssociationSlug: "b", SourceURL: "https://b.example.org", RetrievedAt: reference},
		{AssociationSlug: "c", SourceURL: "https://c.example.org", RetrievedAt: reference},
	}

	first, second := r.Score(bare), r.Score(listed)
	if first.Score != second.Score {
		t.Errorf("membership changed the score: %.1f vs %.1f", first.Score, second.Score)
	}
	if len(first.Reasons) != len(second.Reasons) {
		t.Fatalf("membership changed the reasons")
	}
	for i := range first.Reasons {
		if first.Reasons[i] != second.Reasons[i] {
			t.Errorf("reason %d differs: %+v vs %+v", i, first.Reasons[i], second.Reasons[i])
		}
	}
}

// A company filed under the sector whose own words say otherwise must fall below the bar.
func TestNegativeKeywordsOutweighTheDirectorysFiling(t *testing.T) {
	profile := testProfile()
	r := newTestRanker(t, profile)

	publisher := model.Company{
		ID: "p1", Name: "Pharma Fachzeitschrift GmbH", Country: "de",
		Description: "Fachzeitschrift for the arzneimittel industry.",
		Sectors:     []string{"pharma"}, // the directory filed it under pharma
		Quality:     model.QualityLow,
	}
	ranking := r.Score(publisher)
	if ranking.Score >= profile.MinScore {
		t.Errorf("a trade publisher scored %.1f, at or above min_score %.1f:\n  %s",
			ranking.Score, profile.MinScore, formatReasons(ranking.Reasons))
	}
}

func TestNegativeSkillWeightSubtracts(t *testing.T) {
	r := newTestRanker(t, testProfile())

	clean := strong()
	penalised := strong()
	penalised.Description += " Also offers Lohnabfuellung."

	before, after := r.Score(clean), r.Score(penalised)
	if after.Score >= before.Score {
		t.Errorf("a negative skill did not lower the score: %.1f -> %.1f",
			before.Score, after.Score)
	}
	found := false
	for _, reason := range after.Reasons {
		if reason.Code == ReasonSkillPenalty {
			found = true
			if reason.Points >= 0 {
				t.Errorf("a penalty carried non-negative points: %.2f", reason.Points)
			}
		}
	}
	if !found {
		t.Error("no skill.penalty reason was emitted")
	}
}

// An alias match should say which spelling was found.
func TestAliasMatchNamesTheSpellingFound(t *testing.T) {
	r := newTestRanker(t, testProfile())
	c := strong()
	c.Description = "Arzneimittel manufacturer specialising in drug safety."

	for _, reason := range r.Score(c).Reasons {
		if reason.Code == ReasonSkillMatch && strings.Contains(reason.Detail, "drug safety") {
			return
		}
	}
	t.Errorf("no reason named the alias that matched:\n  %s", formatReasons(r.Score(c).Reasons))
}

func TestScoreIsBounded(t *testing.T) {
	r := newTestRanker(t, testProfile())
	for _, c := range []model.Company{strong(), {ID: "x", Name: "Nichts", Country: "pl"}} {
		ranking := r.Score(c)
		if ranking.Score < 0 || ranking.Score > 100 {
			t.Errorf("%s scored %.1f, outside 0-100", c.Name, ranking.Score)
		}
	}
}

// Rescaling: two profiles expressing different factor sets both reach 100 on a perfect
// company, so min_score means the same thing in both files.
func TestBasisRescalingKeepsThresholdsComparable(t *testing.T) {
	full := testProfile()
	lean := testProfile()
	lean.Languages = nil
	lean.SizeBucket = nil
	lean.Regions = nil

	a, b := newTestRanker(t, full), newTestRanker(t, lean)
	if math.Abs(a.Basis()-b.Basis()) < 0.01 {
		t.Fatal("the two profiles should express different factor sets")
	}

	// Both rankers must agree that a perfect company is a perfect company.
	for _, r := range []*Ranker{a, b} {
		if r.scale <= 0 {
			t.Fatalf("scale is %v", r.scale)
		}
		if total := r.basis * r.scale; math.Abs(total-100) > 0.001 {
			t.Errorf("basis %v rescales to %v, want 100", r.basis, total)
		}
	}
}

func TestBasisReasonIsAlwaysFirstAndCarriesNoPoints(t *testing.T) {
	r := newTestRanker(t, testProfile())
	reasons := r.Score(strong()).Reasons
	if reasons[0].Code != ReasonBasis {
		t.Fatalf("first reason is %q, want %q", reasons[0].Code, ReasonBasis)
	}
	if reasons[0].Points != 0 {
		t.Errorf("the basis reason carried %.2f points, want 0", reasons[0].Points)
	}
}

func TestFreshnessBoundaries(t *testing.T) {
	r := newTestRanker(t, testProfile())
	cases := map[int]string{
		0: "within 30 days", 29: "within 30 days", 30: "within 30 days",
		31: "within 90 days", 89: "within 90 days", 90: "within 90 days",
		91: "within 180 days", 180: "within 180 days", 181: "over 180 days ago",
	}
	for days, want := range cases {
		checked := reference.Add(-time.Duration(days) * 24 * time.Hour)
		c := strong()
		c.LastCheckedAt = &checked
		got := ""
		for _, reason := range r.Score(c).Reasons {
			if reason.Code == ReasonEvidenceFreshness {
				got = reason.Detail
			}
		}
		if !strings.Contains(got, want) {
			t.Errorf("%d days: detail %q does not say %q", days, got, want)
		}
	}
}

func TestBlockedSiteIsNotFresh(t *testing.T) {
	r := newTestRanker(t, testProfile())
	c := strong()
	c.CrawlStatus = model.CrawlBlocked

	for _, reason := range r.Score(c).Reasons {
		if reason.Code == ReasonEvidenceFreshness {
			t.Error("a blocked site was scored as freshly evidenced")
		}
	}
}

func TestRegionAliasesResolve(t *testing.T) {
	r := newTestRanker(t, testProfile())
	for _, spelling := range []string{"Bayern", "Bavaria", "München", "Munich", "by"} {
		c := strong()
		c.Region = spelling
		matched := false
		for _, reason := range r.Score(c).Reasons {
			if reason.Code == ReasonGeoRegion {
				matched = true
			}
		}
		if !matched {
			t.Errorf("region %q did not resolve to the preferred region", spelling)
		}
	}
}

// A country whose policy forbids a general fallback must not score one.
func TestGeneralInboxHonoursTheCountryPolicy(t *testing.T) {
	profile := testProfile()
	profile.Countries = []string{"de", "fr"}
	profile.Regions = nil
	r := newTestRanker(t, profile)

	general := model.Contact{
		Address: "info@example.org", Kind: model.ContactGeneral,
		Verification: model.VerificationPublished, FoundOnURL: "https://example.org/kontakt",
	}
	german := model.Company{ID: "d", Name: "D GmbH", Country: "de",
		Contacts: []model.Contact{general}}
	french := model.Company{ID: "f", Name: "F SAS", Country: "fr",
		Contacts: []model.Contact{general}}

	if !hasCode(r.Score(german).Reasons, ReasonContactGeneral) {
		t.Error("Germany allows a general fallback, so it should have scored")
	}
	if !hasCode(r.Score(french).Reasons, ReasonContactPolicy) {
		t.Error("France forbids a general fallback, so the policy reason should appear")
	}
	if hasCode(r.Score(french).Reasons, ReasonContactGeneral) {
		t.Error("France scored a general inbox its policy forbids")
	}
}

func hasCode(reasons []model.Reason, code string) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func TestRecruitmentBeatsGeneral(t *testing.T) {
	r := newTestRanker(t, testProfile())
	base := strong()
	base.CareersURL = "" // isolate the contact factor

	recruiting := base
	general := base
	general.Contacts = []model.Contact{{
		Address: "info@beispiel.example.org", Kind: model.ContactGeneral,
		Verification: model.VerificationPublished, FoundOnURL: "https://beispiel.example.org/kontakt",
	}}

	if r.Score(general).Score >= r.Score(recruiting).Score {
		t.Error("a general inbox scored at least as well as a recruitment address")
	}
}

func TestUnpublishedContactIsWorthLess(t *testing.T) {
	r := newTestRanker(t, testProfile())
	base := strong()
	base.CareersURL = ""

	unverified := base
	unverified.Contacts = []model.Contact{{
		Address: "karriere@beispiel.example.org", Kind: model.ContactRecruitment,
		Verification: model.VerificationUnverified,
	}}
	if r.Score(unverified).Score >= r.Score(base).Score {
		t.Error("an unverified address scored as well as a published one")
	}
}

func TestScoreAllIsOrderedAndStable(t *testing.T) {
	r := newTestRanker(t, testProfile())
	companies := []model.Company{
		{ID: "zzz", Name: "Weak", Country: "de"},
		strong(),
		{ID: "aaa", Name: "Weak", Country: "de"},
	}
	ranked := r.ScoreAll(companies)
	if ranked[0].CompanyID != "c1" {
		t.Errorf("top company is %q, want c1", ranked[0].CompanyID)
	}
	// Equal scores break ties on id, so output is byte-stable.
	if ranked[1].CompanyID != "aaa" || ranked[2].CompanyID != "zzz" {
		t.Errorf("ties not broken on id: %q then %q", ranked[1].CompanyID, ranked[2].CompanyID)
	}
}

func TestNewRejectsUnusableProfiles(t *testing.T) {
	bundle := testBundle()
	cases := map[string]model.Profile{
		"no preference": {Slug: "empty"},
		"unknown sector": {Slug: "s", Sectors: []string{"nope"},
			Skills: []model.SkillWeight{{Term: "x", Weight: 1}}},
		"unknown country": {Slug: "c", Countries: []string{"xx"}},
		"no positive weight": {Slug: "w", Countries: []string{"de"},
			Skills: []model.SkillWeight{{Term: "x", Weight: -1}}},
		"empty skill term": {Slug: "t", Countries: []string{"de"},
			Skills: []model.SkillWeight{{Term: "   ", Weight: 1}}},
	}
	for name, profile := range cases {
		if _, err := New(profile, Options{Bundle: bundle, Now: reference}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestNewRequiresABundleAndAClock(t *testing.T) {
	if _, err := New(testProfile(), Options{Now: reference}); err == nil {
		t.Error("a missing bundle should be an error")
	}
	if _, err := New(testProfile(), Options{Bundle: testBundle()}); err == nil {
		t.Error("a zero Now should be an error, or freshness is not reproducible")
	}
}

func TestSignificantDropsOnlyZeroPointReasons(t *testing.T) {
	reasons := []model.Reason{
		{Code: "a", Points: 1.5}, {Code: "b", Points: 0}, {Code: "c", Points: -2},
	}
	got := Significant(reasons)
	if len(got) != 2 || got[0].Code != "a" || got[1].Code != "c" {
		t.Errorf("Significant = %+v", got)
	}
}

func TestWeightsValidate(t *testing.T) {
	if err := DefaultWeights().Validate(); err != nil {
		t.Fatalf("the shipped table is invalid: %v", err)
	}
	bad := DefaultWeights()
	bad.Skills = 10
	if err := bad.Validate(); err == nil {
		t.Error("a table that does not sum to 100 should be rejected")
	}
	declared := DefaultWeights()
	declared.SectorDeclaredMax = declared.Sector + 1
	if err := declared.Validate(); err == nil {
		t.Error("a directory's filing outweighing the vocabulary should be rejected")
	}
}
