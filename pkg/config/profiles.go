package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// SkillWeight is one scored requirement, as written in a profile file.
//
// Weight is relative, not absolute: the skills factor scores the share of a profile's total
// positive weight that matched, so doubling every weight in a file changes nothing. That is
// the right affordance for a human editing YAML — what matters is which terms outweigh
// which, not the arithmetic.
type SkillWeight struct {
	Term    string   `yaml:"term"`
	Weight  float64  `yaml:"weight"`
	Aliases []string `yaml:"aliases,omitempty"`
}

// Profile is a ranking profile as configured.
//
// It holds preferences and nothing else: skills, sectors and places. No CV, no name, no
// contact details. That is not an oversight — this repository is public, and a profile that
// needed redacting could not live here at all.
type Profile struct {
	Slug      string        `yaml:"slug"`
	Headline  string        `yaml:"headline"`
	Countries []string      `yaml:"countries,omitempty"`
	Regions   []string      `yaml:"regions,omitempty"`
	Sectors   []string      `yaml:"sectors,omitempty"`
	Languages []string      `yaml:"languages,omitempty"`
	Skills    []SkillWeight `yaml:"skills,omitempty"`
	MustHave  []string      `yaml:"must_have,omitempty"`
	Exclude   []string      `yaml:"exclude,omitempty"`
	// SizeBuckets matches model.Profile.SizeBucket, whose JSON name is already
	// "size_buckets". The YAML name follows the JSON one so the two spellings agree.
	SizeBuckets []string `yaml:"size_buckets,omitempty"`
	MinScore    float64  `yaml:"min_score,omitempty"`

	sourceFile string
}

// SourceFile is the path the profile was loaded from, for error messages.
func (p Profile) SourceFile() string { return p.sourceFile }

// Model converts to the runtime type the ranker consumes.
func (p Profile) Model() model.Profile {
	skills := make([]model.SkillWeight, 0, len(p.Skills))
	for _, skill := range p.Skills {
		skills = append(skills, model.SkillWeight{
			Term:    skill.Term,
			Weight:  skill.Weight,
			Aliases: skill.Aliases,
		})
	}
	return model.Profile{
		Slug:       p.Slug,
		Headline:   p.Headline,
		Countries:  p.Countries,
		Regions:    p.Regions,
		Sectors:    p.Sectors,
		Languages:  p.Languages,
		Skills:     skills,
		MustHave:   p.MustHave,
		Exclude:    p.Exclude,
		SizeBucket: p.SizeBuckets,
		MinScore:   p.MinScore,
	}
}

// SizeBuckets a profile may ask for. Kept here rather than in the ranker because it is a
// configuration vocabulary, and `config validate` is where a typo should be caught.
var validSizeBuckets = map[string]bool{
	"micro": true, "small": true, "medium": true, "large": true,
}

// normalizeTerm reduces a configured term to the form the ranker matches with.
//
// It must agree exactly with Sector.AllTerms(), which does the same lowering and accent
// stripping — otherwise a sector term and a skill term with the same spelling would behave
// differently, which is the kind of divergence nobody finds for months.
func normalizeTerm(term string) string {
	return strings.ToLower(normalize.StripAccents(strings.TrimSpace(term)))
}

// validateProfiles checks every profile against the rest of the configuration.
//
// Like the rest of this package it collects problems rather than stopping at the first:
// a profile is edited by hand and usually has several things wrong at once.
func (b *Bundle) validateProfiles() []Problem {
	var problems []Problem
	add := func(file, format string, args ...any) {
		problems = append(problems, Problem{file, fmt.Sprintf(format, args...)})
	}

	for slug, profile := range b.Profiles {
		file := profile.sourceFile

		if !slugPattern(slug) {
			add(file, "profile slug %q must be lowercase letters, digits, - or _", slug)
		}
		// Stricter than associations on purpose: `eucd rank --profile <slug>` sends an
		// operator looking for a file with that name.
		if stem := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)); stem != slug {
			add(file, "profile slug %q does not match the filename %q; "+
				"rank --profile %s should find this file by name", slug, stem, slug)
		}
		if strings.TrimSpace(profile.Headline) == "" {
			add(file, "profile %q has no headline; it is what `eucd profiles list` and "+
				"`eucd rank` print, and an unnamed profile cannot be reviewed", slug)
		}

		for _, code := range profile.Countries {
			if _, ok := b.Countries[code]; !ok {
				add(file, "profile %q names country %q, which is not configured in %s/countries",
					slug, code, b.Dir)
			}
		}
		for _, sectorSlug := range profile.Sectors {
			if _, ok := b.Sectors[sectorSlug]; !ok {
				add(file, "profile %q names sector %q, which is not configured in %s/sectors",
					slug, sectorSlug, b.Dir)
			}
		}

		// A region only means something inside a country, so it is checked against the
		// countries this profile actually listed.
		known := map[string]bool{}
		for _, code := range profile.Countries {
			for _, region := range b.Countries[code].Regions {
				known[region.Code] = true
			}
		}
		for _, region := range profile.Regions {
			if len(profile.Countries) == 0 {
				add(file, "profile %q lists region %q but names no country, so there is "+
					"nothing to resolve it against", slug, region)
				break
			}
			if !known[region] {
				add(file, "profile %q names region %q, which none of its countries (%s) defines",
					slug, region, strings.Join(profile.Countries, ", "))
			}
		}

		for _, language := range profile.Languages {
			if len(language) != 2 || !slugPattern(language) {
				add(file, "profile %q: language %q must be two lowercase letters", slug, language)
			}
		}
		for _, bucket := range profile.SizeBuckets {
			if !validSizeBuckets[bucket] {
				add(file, "profile %q: size bucket %q must be one of micro, small, medium, large",
					slug, bucket)
			}
		}
		if profile.MinScore < 0 || profile.MinScore > 100 {
			add(file, "profile %q: min_score %.1f is outside 0-100, and scores run 0-100",
				slug, profile.MinScore)
		}

		// A profile expressing no preference scores every company identically, which is a
		// silently useless run rather than an error at use time.
		if len(profile.Skills) == 0 && len(profile.Sectors) == 0 && len(profile.Countries) == 0 {
			add(file, "profile %q expresses no preference at all (no skills, sectors or "+
				"countries), so every company would score the same", slug)
		}

		seenTerms := map[string]string{}
		positiveWeight := 0.0
		for index, skill := range profile.Skills {
			term := normalizeTerm(skill.Term)
			if term == "" {
				add(file, "profile %q: skill %d has an empty term", slug, index+1)
				continue
			}
			if previous, clash := seenTerms[term]; clash {
				add(file, "profile %q lists the term %q twice (also as %q); "+
					"it would be counted twice", slug, skill.Term, previous)
			}
			seenTerms[term] = skill.Term

			if skill.Weight == 0 {
				add(file, "profile %q: skill %q has weight 0, so it is never counted; "+
					"remove it or give it a weight", slug, skill.Term)
			}
			if skill.Weight > 0 {
				positiveWeight += skill.Weight
			}
			for _, alias := range skill.Aliases {
				if normalizeTerm(alias) == term {
					add(file, "profile %q: skill %q lists itself as an alias", slug, skill.Term)
				}
			}
		}
		if len(profile.Skills) > 0 && positiveWeight <= 0 {
			add(file, "profile %q has skills but no positive weight among them, so the "+
				"skills score has nothing to divide by", slug)
		}

		excluded := map[string]bool{}
		for _, term := range profile.Exclude {
			normalized := normalizeTerm(term)
			if normalized == "" {
				add(file, "profile %q has an empty exclude term", slug)
				continue
			}
			excluded[normalized] = true
		}
		// An excluded term always wins, so a skill naming it can never contribute.
		for _, skill := range profile.Skills {
			if skill.Weight > 0 && excluded[normalizeTerm(skill.Term)] {
				add(file, "profile %q lists %q as both a scored skill and an exclude term; "+
					"exclude always wins, so the skill can never count", slug, skill.Term)
			}
		}
		for _, term := range profile.MustHave {
			normalized := normalizeTerm(term)
			if normalized == "" {
				add(file, "profile %q has an empty must_have term", slug)
				continue
			}
			if excluded[normalized] {
				add(file, "profile %q lists %q as both must_have and exclude, so no company "+
					"could ever score", slug, term)
			}
		}
	}
	return problems
}
