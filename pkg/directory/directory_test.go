package directory

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/iobyte-labs/eu-company-discovery/pkg/model"
)

type fakeAdapter struct{ name string }

func (f fakeAdapter) Name() string { return f.name }
func (fakeAdapter) Discover(context.Context, Request) (Result, error) {
	return Result{}, nil
}

// The seam: a package the engine has never heard of registers an adapter, and the engine
// finds it by name. This is what lets an adapter live in its own repository.
func TestRegisterMakesAnAdapterDiscoverable(t *testing.T) {
	const name = "test_external_pack"
	Register(name, func() Adapter { return fakeAdapter{name} })
	t.Cleanup(func() { unregisterForTest(name) })

	if !slices.Contains(Names(), name) {
		t.Fatalf("Names() = %v, missing %q", Names(), name)
	}
	adapter, err := Get(name)
	if err != nil {
		t.Fatalf("Get(%q): %v", name, err)
	}
	if adapter.Name() != name {
		t.Errorf("adapter name = %q, want %q", adapter.Name(), name)
	}
}

func TestGetUnknownAdapterNamesWhatIsAvailable(t *testing.T) {
	_, err := Get("no_such_adapter")
	if err == nil {
		t.Fatal("Get should fail for an unregistered adapter")
	}
	if !strings.Contains(err.Error(), SeedListName) {
		t.Errorf("the error should list available adapters, got: %v", err)
	}
}

// Each run gets a fresh adapter, so state cannot leak between associations.
func TestGetReturnsAFreshAdapter(t *testing.T) {
	const name = "test_counting_pack"
	built := 0
	Register(name, func() Adapter { built++; return fakeAdapter{name} })
	t.Cleanup(func() { unregisterForTest(name) })

	if _, err := Get(name); err != nil {
		t.Fatal(err)
	}
	if _, err := Get(name); err != nil {
		t.Fatal(err)
	}
	if built != 2 {
		t.Errorf("factory called %d times, want 2", built)
	}
}

func TestRegisterRejectsDuplicateNames(t *testing.T) {
	const name = "test_duplicate_pack"
	Register(name, func() Adapter { return fakeAdapter{name} })
	t.Cleanup(func() { unregisterForTest(name) })

	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate name should panic rather than shadow silently")
		}
	}()
	Register(name, func() Adapter { return fakeAdapter{name} })
}

// A factory whose adapter disagrees with its registered name would make `discover` depend
// on import order.
func TestGetRejectsNameMismatch(t *testing.T) {
	const name = "test_mismatched_pack"
	Register(name, func() Adapter { return fakeAdapter{"something_else"} })
	t.Cleanup(func() { unregisterForTest(name) })

	if _, err := Get(name); err == nil {
		t.Error("Get should reject an adapter that reports a different name")
	}
}

func TestSeedListIsRegisteredByDefault(t *testing.T) {
	if _, err := Get(SeedListName); err != nil {
		t.Fatalf("the built-in seed_list adapter is not registered: %v", err)
	}
}

func seedRequest(settings map[string]any) Request {
	return Request{Association: model.Association{
		Slug:         "fr_test",
		Country:      "fr",
		Sectors:      []string{"automotive"},
		DirectoryURL: "https://assoc.example.fr/members",
		Adapter:      SeedListName,
		AccessMethod: model.AccessManual,
		Settings:     settings,
	}}
}

func TestSeedListReadsCuratedEntries(t *testing.T) {
	req := seedRequest(map[string]any{
		"source_note": "2026 member yearbook, p. 44",
		"companies": []any{
			map[string]any{
				"name":        "Acme Industries SAS",
				"website":     "https://acme.example.fr",
				"region":      "idf",
				"description": "Stamped metal parts.",
			},
			map[string]any{"name": "Beta Mécanique"},
		},
	})

	result, err := SeedListAdapter{}.Discover(context.Background(), req)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("got %d records, want 2", len(result.Records))
	}
	if result.PagesFetched != 0 {
		t.Errorf("a manual adapter must fetch nothing, got %d pages", result.PagesFetched)
	}

	first := result.Records[0]
	if first.RawName != "Acme Industries SAS" {
		t.Errorf("name = %q", first.RawName)
	}
	if first.Country != "fr" {
		t.Errorf("country should default to the association's, got %q", first.Country)
	}
	if !slices.Equal(first.Sectors, []string{"automotive"}) {
		t.Errorf("sectors should default to the association's, got %v", first.Sectors)
	}
	if first.SourceURL != "https://assoc.example.fr/members" {
		t.Errorf("source url = %q, want the directory url", first.SourceURL)
	}
	if first.RetrievedAt.IsZero() {
		t.Error("retrieved_at must be set, or provenance is incomplete")
	}
	if first.Extra["source_note"] != "2026 member yearbook, p. 44" {
		t.Errorf("source_note was lost: %v", first.Extra)
	}

	// An entry with no website is kept, but says so.
	second := result.Records[1]
	if second.Incomplete == "" {
		t.Error("an entry with no website should report what is missing")
	}
}

func TestSeedListReportsBadEntriesWithoutDroppingGoodOnes(t *testing.T) {
	req := seedRequest(map[string]any{"companies": []any{
		map[string]any{"name": "Good SAS", "website": "https://good.example.fr"},
		map[string]any{"description": "nameless"},
		"not a mapping",
		map[string]any{"name": "Bad URL SAS", "website": "javascript:alert(1)"},
	}})

	result, err := SeedListAdapter{}.Discover(context.Background(), req)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("got %d usable records, want 2", len(result.Records))
	}
	if len(result.Warnings) != 3 {
		t.Errorf("warnings = %v, want 3 (nameless, non-mapping, bad url)", result.Warnings)
	}
	// The unsafe URL is dropped, not carried through.
	for _, record := range result.Records {
		if strings.HasPrefix(record.Website, "javascript:") {
			t.Error("an unsafe website survived")
		}
	}
}

func TestSeedListHonoursMaxRecords(t *testing.T) {
	req := seedRequest(map[string]any{"companies": []any{
		map[string]any{"name": "One"},
		map[string]any{"name": "Two"},
		map[string]any{"name": "Three"},
	}})
	req.Limits = model.CrawlLimits{MaxRecords: 2}

	result, err := SeedListAdapter{}.Discover(context.Background(), req)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Records) != 2 {
		t.Errorf("got %d records, want 2", len(result.Records))
	}
	if !result.Truncated {
		t.Error("a capped run must report Truncated, or it looks complete")
	}
}

func TestSeedListFailsLoudlyOnMissingConfig(t *testing.T) {
	cases := map[string]map[string]any{
		"no companies key": {"source_note": "x"},
		"empty list":       {"companies": []any{}},
		"all unusable":     {"companies": []any{map[string]any{"description": "nameless"}}},
	}
	adapter := SeedListAdapter{}
	for name, settings := range cases {
		if _, err := adapter.Discover(context.Background(), seedRequest(settings)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSettingHelpers(t *testing.T) {
	settings := map[string]any{
		"text":   "value",
		"empty":  "",
		"number": 7,
		"float":  7.0,
		"wrong":  []any{1},
	}
	if got := SettingString(settings, "text", "fallback"); got != "value" {
		t.Errorf("SettingString = %q", got)
	}
	if got := SettingString(settings, "empty", "fallback"); got != "fallback" {
		t.Errorf("an empty string should fall back, got %q", got)
	}
	if got := SettingString(settings, "absent", "fallback"); got != "fallback" {
		t.Errorf("SettingString(absent) = %q", got)
	}
	if got := SettingString(settings, "wrong", "fallback"); got != "fallback" {
		t.Errorf("a wrongly typed value should fall back, got %q", got)
	}
	if got := SettingInt(settings, "number", 0); got != 7 {
		t.Errorf("SettingInt(int) = %d", got)
	}
	if got := SettingInt(settings, "float", 0); got != 7 {
		t.Errorf("SettingInt(float64) = %d", got)
	}
	if got := SettingInt(settings, "absent", 3); got != 3 {
		t.Errorf("SettingInt(absent) = %d", got)
	}
}
