package rank

import (
	"regexp"
	"strings"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
	"github.com/iobyte-labs/eu-company-discovery/pkg/normalize"
)

// field is one searchable part of a company record, in the order it is searched.
type field struct {
	name string
	text string // normalised and padded, ready for substring matching
}

// Document is the text of a company that terms are matched against.
//
// It holds what the company said about itself: its name, its description, its keywords and
// its sectors. It deliberately does NOT include the website, canonical domain or careers
// URL. A company at recrutement-auto.fr would otherwise match a recruitment skill term
// because of its domain name, which is not evidence of anything.
type Document struct {
	fields []field
}

var nonAlnum = regexp.MustCompile(`[^0-9a-z]+`)

// normalizeText lowercases, strips accents, reduces every run of non-alphanumeric
// characters to a single space, and pads the result.
//
// The padding is what makes word-boundary matching work with plain substring search: a term
// is searched for as " term ", so "auto" does not match "automotive". That is the same
// class of bug the store's sector filter already guards against.
func normalizeText(text string) string {
	lowered := strings.ToLower(normalize.StripAccents(text))
	collapsed := strings.TrimSpace(nonAlnum.ReplaceAllString(lowered, " "))
	if collapsed == "" {
		return ""
	}
	return " " + collapsed + " "
}

// normalizeTerm reduces a search term to the same shape, without the padding.
//
// It must agree with config.Sector.AllTerms(), which lowercases and strips accents the same
// way, or a sector term and a skill term with identical spelling would behave differently.
func normalizeTerm(term string) string {
	lowered := strings.ToLower(normalize.StripAccents(term))
	return strings.TrimSpace(nonAlnum.ReplaceAllString(lowered, " "))
}

// NewDocument builds the searchable text of a company.
func NewDocument(c model.Company) Document {
	parts := []struct {
		name string
		text string
	}{
		{"name", c.Name},
		{"description", c.Description},
		{"keywords", strings.Join(c.Keywords, " ")},
		{"sectors", strings.Join(c.Sectors, " ")},
	}
	fields := make([]field, 0, len(parts))
	for _, part := range parts {
		if text := normalizeText(part.text); text != "" {
			fields = append(fields, field{name: part.name, text: text})
		}
	}
	return Document{fields: fields}
}

// Find reports whether a term appears, and in which field.
//
// Fields are searched in a fixed order so that the field named in a reason is deterministic.
// Multi-word terms work for free, because both sides collapse their separators to single
// spaces: "metal stamping" matches "Metal-Stamping" and "metal   stamping" alike.
func (d Document) Find(term string) (string, bool) {
	needle := normalizeTerm(term)
	if needle == "" {
		return "", false
	}
	padded := " " + needle + " "
	for _, f := range d.fields {
		if strings.Contains(f.text, padded) {
			return f.name, true
		}
	}
	return "", false
}

// FindAny returns the first of terms that appears, with the field it appeared in.
// Terms are tried in the order given, so the caller controls which match is reported.
func (d Document) FindAny(terms []string) (string, string, bool) {
	for _, term := range terms {
		if where, ok := d.Find(term); ok {
			return term, where, true
		}
	}
	return "", "", false
}

// CountDistinct returns how many of terms appear at least once. Used where the evidence is
// "several independent vocabulary hits" rather than any single term.
func (d Document) CountDistinct(terms []string) int {
	found := 0
	for _, term := range terms {
		if _, ok := d.Find(term); ok {
			found++
		}
	}
	return found
}

// Empty reports whether there is nothing to search.
func (d Document) Empty() bool { return len(d.fields) == 0 }
