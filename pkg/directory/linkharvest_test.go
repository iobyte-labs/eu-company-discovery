package directory

import (
	"context"
	"strings"
	"testing"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

func linkedAssociation(settings map[string]any) model.Association {
	return model.Association{
		Slug:         "de_verband",
		Name:         "Ein Verband",
		Country:      "de",
		Sectors:      []string{"pharma"},
		DirectoryURL: "https://verband.example.org/mitglieder",
		Adapter:      LinkHarvestName,
		AccessMethod: model.AccessHTML,
		Settings:     settings,
	}
}

func harvest(t *testing.T, settings map[string]any, limits model.CrawlLimits) Result {
	t.Helper()
	fetcher := &stubFetcher{pages: map[string]string{
		"https://verband.example.org/mitglieder": "members/linked.html",
	}}
	result, err := LinkHarvestAdapter{}.Discover(context.Background(), Request{
		Association: linkedAssociation(settings),
		Fetcher:     fetcher,
		Limits:      limits,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return result
}

func harvestedNames(result Result) map[string]model.DirectoryRecord {
	out := map[string]model.DirectoryRecord{}
	for _, record := range result.Records {
		out[record.RawName] = record
	}
	return out
}

func TestLinkHarvestFindsTheMembersAndNotTheFurniture(t *testing.T) {
	result := harvest(t, nil, model.CrawlLimits{})
	byName := harvestedNames(result)

	for _, want := range []string{
		"Beispiel Pharma GmbH",
		"Beispiel Biotech AG",
		"Beispiel Labor GmbH & Co. KG", // linked twice; the real name beats "Website"
		"Beispiel Medizingeräte SE",
	} {
		if _, ok := byName[want]; !ok {
			t.Errorf("member %q was not harvested; got %v", want, namesOf(result))
		}
	}

	// Scoping to the densest block of links is what keeps the sponsor strip, the social
	// row and the web agency out without anyone writing a selector.
	for _, unwanted := range []string{"Sponsor Bank", "Sponsor Messe", "Umsetzung: Webagentur"} {
		if _, found := byName[unwanted]; found {
			t.Errorf("%q was harvested as a member", unwanted)
		}
	}
	for _, record := range result.Records {
		for _, social := range []string{"linkedin", "twitter"} {
			if strings.Contains(record.Website, social) {
				t.Errorf("a social media link was harvested as a member: %q", record.Website)
			}
		}
		// The directory never lists itself.
		if strings.Contains(record.Website, "verband.example.org") {
			t.Errorf("the association's own site was harvested as a member: %q", record.Website)
		}
	}
}

func namesOf(result Result) []string {
	out := make([]string, 0, len(result.Records))
	for _, record := range result.Records {
		out = append(out, record.RawName)
	}
	return out
}

func TestLinkHarvestSaysEveryRecordIsAGuess(t *testing.T) {
	// The adapter cannot tell a member from a supplier, so every record it produces has to
	// admit how it was found. A reviewer sorting by this column sees exactly which rows
	// need a human eye.
	result := harvest(t, nil, model.CrawlLimits{})
	if len(result.Records) == 0 {
		t.Fatal("nothing was harvested")
	}
	for _, record := range result.Records {
		if !strings.Contains(record.Incomplete, "harvested") {
			t.Errorf("%q does not say it was harvested: %q", record.RawName, record.Incomplete)
		}
		if record.Extra["harvested_from_links"] != "true" {
			t.Errorf("%q carries no harvested marker in Extra", record.RawName)
		}
	}
	// The counts behind the result are reported, including what the page was scoped to.
	joined := strings.Join(result.Warnings, " ")
	if !strings.Contains(joined, "outbound link") || !strings.Contains(joined, "scoped to") {
		t.Errorf("warnings = %v, want the link counts and the container chosen", result.Warnings)
	}
}

func TestLinkHarvestUsesTheDomainWhenThePageGivesNoName(t *testing.T) {
	// "Website" is not a company name. Inventing one would be worse than using the domain,
	// because the first name a company is seen under is the one it keeps.
	result := harvest(t, nil, model.CrawlLimits{})
	byName := harvestedNames(result)

	record, ok := byName["beispiel-verpackung.example"]
	if !ok {
		t.Fatalf("the unnamed member was not harvested under its domain; got %v", namesOf(result))
	}
	if !strings.Contains(record.Incomplete, "no company name") {
		t.Errorf("Incomplete = %q, want it to say the page gave no name", record.Incomplete)
	}
	if _, wrong := byName["Website"]; wrong {
		t.Error(`a member was recorded with the name "Website"`)
	}
}

func TestLinkHarvestCountsADomainOnceHoweverOftenItIsLinked(t *testing.T) {
	result := harvest(t, nil, model.CrawlLimits{})
	seen := map[string]int{}
	for _, record := range result.Records {
		seen[record.Website]++
	}
	for website, count := range seen {
		if count > 1 {
			t.Errorf("%s produced %d records, want 1", website, count)
		}
	}
	// The link count is kept, because a domain linked many times is more often the site
	// builder than a member.
	byName := harvestedNames(result)
	if got := byName["Beispiel Labor GmbH & Co. KG"].Extra["link_count"]; got != "2" {
		t.Errorf("link_count = %q for the twice-linked member, want 2", got)
	}
}

func TestLinkHarvestHonoursAnExplicitContainer(t *testing.T) {
	result := harvest(t, map[string]any{"container_selector": "aside.sponsors"},
		model.CrawlLimits{})
	byName := harvestedNames(result)
	if _, ok := byName["Sponsor Bank"]; !ok {
		t.Errorf("an explicit container was ignored; got %v", namesOf(result))
	}
	if _, ok := byName["Beispiel Pharma GmbH"]; ok {
		t.Error("the member list was harvested despite the container naming the sponsors")
	}
}

func TestLinkHarvestFailsWhenAPageLinksToNobody(t *testing.T) {
	// The failure this must not hide: a page that cannot be read this way looks exactly
	// like an association with no members.
	empty := `<html><body><h1>Mitglieder</h1><p>Bitte rufen Sie uns an.</p>
	  <a href="/impressum">Impressum</a></body></html>`
	fetcher := &stubFetcher{pages: map[string]string{
		"https://verband.example.org/mitglieder": empty,
	}}
	_, err := LinkHarvestAdapter{}.Discover(context.Background(), Request{
		Association: linkedAssociation(nil),
		Fetcher:     fetcher,
	})
	if err == nil {
		t.Fatal("a page with no outbound company links reported success")
	}
	if !strings.Contains(err.Error(), "no outbound company links") {
		t.Errorf("err = %v", err)
	}
}

func TestLinkHarvestIsRegistered(t *testing.T) {
	adapter, err := Get(LinkHarvestName)
	if err != nil {
		t.Fatalf("Get(%q): %v", LinkHarvestName, err)
	}
	if adapter.Name() != LinkHarvestName {
		t.Errorf("Name() = %q", adapter.Name())
	}
}
