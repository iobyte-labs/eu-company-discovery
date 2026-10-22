package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bundleForProfiles() *Bundle {
	return &Bundle{
		Dir: "config",
		Countries: map[string]Country{
			"de": {Code: "de", Name: "Germany", Languages: []string{"de"},
				Regions: []Region{{Code: "by", Name: "Bayern"}}},
		},
		Sectors:  map[string]Sector{"pharma": {Slug: "pharma", Name: "Pharma", Keywords: []string{"arzneimittel"}}},
		Profiles: map[string]Profile{},
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The seam a CV-derived profile arrives through: a file that is not in this repository.
func TestLoadProfileFileAcceptsJSON(t *testing.T) {
	path := writeTemp(t, "derived.json", `{
	  "slug": "derived",
	  "headline": "Derived from a CV",
	  "countries": ["de"],
	  "sectors": ["pharma"],
	  "skills": [{"term": "pharmakovigilanz", "weight": 10, "aliases": ["drug safety"]}],
	  "min_score": 30
	}`)
	profile, err := LoadProfileFile(path, bundleForProfiles())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if profile.Slug != "derived" || len(profile.Skills) != 1 {
		t.Fatalf("unexpected profile: %+v", profile)
	}
	if profile.Skills[0].Weight != 10 || profile.Skills[0].Aliases[0] != "drug safety" {
		t.Errorf("skill did not survive the round trip: %+v", profile.Skills[0])
	}
}

func TestLoadProfileFileAcceptsYAML(t *testing.T) {
	path := writeTemp(t, "derived.yaml", "slug: derived\nheadline: From a CV\ncountries: [de]\nsectors: [pharma]\n")
	if _, err := LoadProfileFile(path, bundleForProfiles()); err != nil {
		t.Fatalf("load: %v", err)
	}
}

// A file with no slug takes its name from the filename, which is what an operator will
// recognise and what a generator need not invent.
func TestProfileFileSlugDefaultsToTheFilename(t *testing.T) {
	path := writeTemp(t, "from-my-cv.json", `{"headline": "x", "countries": ["de"]}`)
	profile, err := LoadProfileFile(path, bundleForProfiles())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if profile.Slug != "from-my-cv" {
		t.Errorf("slug = %q, want from-my-cv", profile.Slug)
	}
}

// A generated file with a misspelled key must fail loudly, or it scores against a
// preference nobody expressed.
func TestLoadProfileFileRejectsUnknownFields(t *testing.T) {
	path := writeTemp(t, "typo.json", `{"slug": "t", "headline": "x", "countries": ["de"], "skils": []}`)
	_, err := LoadProfileFile(path, bundleForProfiles())
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "skils") {
		t.Errorf("the error should name the offending key, got: %v", err)
	}
}

// Validated against the real configuration, so a profile naming a sector this build does
// not have fails on load rather than silently scoring nothing.
func TestLoadProfileFileValidatesAgainstConfiguration(t *testing.T) {
	path := writeTemp(t, "bad.json", `{"slug": "b", "headline": "x", "sectors": ["nonexistent"]}`)
	if _, err := LoadProfileFile(path, bundleForProfiles()); err == nil {
		t.Fatal("an unknown sector was accepted")
	}
}

func TestLoadProfileFileRejectsOtherExtensions(t *testing.T) {
	path := writeTemp(t, "cv.pdf", "not a profile")
	if _, err := LoadProfileFile(path, bundleForProfiles()); err == nil {
		t.Fatal("a .pdf was accepted as a profile")
	}
}

func TestLoadProfileFileReportsAMissingFile(t *testing.T) {
	_, err := LoadProfileFile(filepath.Join(t.TempDir(), "absent.json"), bundleForProfiles())
	if err == nil || !strings.Contains(err.Error(), "no profile file") {
		t.Errorf("expected a clear missing-file error, got: %v", err)
	}
}

// A profile is written and read back unchanged, which is how a private consumer hands one
// over.
func TestWriteProfileFileRoundTrips(t *testing.T) {
	bundle := bundleForProfiles()
	source := writeTemp(t, "a.json", `{
	  "slug": "a", "headline": "h", "countries": ["de"], "sectors": ["pharma"],
	  "skills": [{"term": "x", "weight": 2.5}], "min_score": 12.5
	}`)
	profile, err := LoadProfileFile(source, bundle)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.json")
	if err := WriteProfileFile(out, profile); err != nil {
		t.Fatal(err)
	}
	again, err := LoadProfileFile(out, bundle)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if again.Slug != profile.Slug || again.MinScore != profile.MinScore ||
		len(again.Skills) != len(profile.Skills) || again.Skills[0].Weight != 2.5 {
		t.Errorf("round trip changed the profile: %+v vs %+v", again, profile)
	}
}

// The committed example must stay valid: a shipped profile that fails validation is the
// worst possible first impression, and `make validate` has to keep passing.
func TestCommittedProfilesAreValid(t *testing.T) {
	bundle, err := Load("../../config", []string{"seed_list", "html_directory", "exec", "link_harvest"})
	if err != nil {
		t.Fatalf("the committed configuration does not validate: %v", err)
	}
	if len(bundle.Profiles) == 0 {
		t.Fatal("no profiles are committed, so this test proves nothing")
	}
}
