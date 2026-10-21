package normalize

import "testing"

func TestCompanyName(t *testing.T) {
	cases := map[string]string{
		"  Établissements Dupont & Fils S.A.S. ": "etablissements dupont fils sas",
		"RENAULT SAS":            "renault sas",
		"Renault S.A.S.":         "renault sas",
		"L'Oréal":                "loreal",
		"Müller GmbH & Co. KG":   "muller gmbh co kg",
		"ACME   \t Industries\n": "acme industries",
		"Søren Ørsted A/S":       "soren orsted a s",
		"":                       "",
	}
	for input, want := range cases {
		if got := CompanyName(input); got != want {
			t.Errorf("CompanyName(%q) = %q, want %q", input, got, want)
		}
	}
}

// The legal form carries meaning, so normalisation must not erase it. If this test ever
// fails, dedup has started merging companies it has no business merging.
func TestCompanyNameKeepsLegalForm(t *testing.T) {
	if CompanyName("Renault SA") == CompanyName("Renault SAS") {
		t.Fatal("Renault SA and Renault SAS normalised to the same name")
	}
}

func TestRegistrableDomain(t *testing.T) {
	cases := map[string]string{
		"www.renault.fr":          "renault.fr",
		"shop.renault.co.uk":      "renault.co.uk",
		"renault.co.uk":           "renault.co.uk",
		"a.b.c.acme.com":          "acme.com",
		"acme.com":                "acme.com",
		"localhost":               "",
		"192.168.1.1":             "",
		"::1":                     "",
		"":                        "",
		"deep.sub.acme.gitlab.io": "acme.gitlab.io",
	}
	for input, want := range cases {
		if got := RegistrableDomain(input); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCanonicalDomain(t *testing.T) {
	cases := map[string]string{
		"https://www.acme.fr/careers?x=1": "acme.fr",
		"http://ACME.FR":                  "acme.fr",
		"www.acme.fr/careers":             "acme.fr",
		"acme.fr":                         "acme.fr",
		"mailto:info@acme.fr":             "",
		"":                                "",
	}
	for input, want := range cases {
		if got := CanonicalDomain(input); got != want {
			t.Errorf("CanonicalDomain(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestURL(t *testing.T) {
	cases := map[string]string{
		"https://WWW.Acme.FR:443/Careers/":             "https://www.acme.fr/Careers",
		"http://acme.fr:80/":                           "http://acme.fr",
		"https://acme.fr/a//b/":                        "https://acme.fr/a/b",
		"https://acme.fr/p?utm_source=x&id=7&fbclid=z": "https://acme.fr/p?id=7",
		"https://acme.fr/p?b=2&a=1":                    "https://acme.fr/p?a=1&b=2",
		"https://acme.fr/p#section":                    "https://acme.fr/p",
		"acme.fr/jobs":                                 "https://acme.fr/jobs",
		"ftp://acme.fr/x":                              "",
		"javascript:alert(1)":                          "",
		"mailto:a@b.fr":                                "",
		"":                                             "",
		"https://acme.fr/p?utm_source=x":               "https://acme.fr/p",
	}
	for input, want := range cases {
		if got := URL(input); got != want {
			t.Errorf("URL(%q) = %q, want %q", input, got, want)
		}
	}
}

// Two spellings of one page must normalise to one string, or the fetcher will request it
// twice and the per-domain budget will be spent on duplicates.
func TestURLIsIdempotent(t *testing.T) {
	for _, raw := range []string{
		"https://WWW.Acme.FR:443/Careers/?utm_source=x&id=7",
		"https://acme.fr/a//b/",
		"acme.fr/jobs",
	} {
		once := URL(raw)
		if twice := URL(once); twice != once {
			t.Errorf("URL not idempotent for %q: %q then %q", raw, once, twice)
		}
	}
}

func TestSameSite(t *testing.T) {
	if !SameSite("https://www.acme.fr/a", "http://jobs.acme.fr/b") {
		t.Error("subdomains of one registrable domain should be the same site")
	}
	if SameSite("https://acme.fr", "https://acme.de") {
		t.Error("different TLDs are different sites")
	}
	if SameSite("not a url", "also not") {
		t.Error("unparseable input must not count as the same site")
	}
}

func TestEmail(t *testing.T) {
	cases := map[string]string{
		"  Info@ACME.FR ":                "info@acme.fr",
		"<jobs@acme.fr>":                 "jobs@acme.fr",
		"info (at) acme [dot] fr":        "info@acme.fr",
		"recrutement (AT) acme (DOT) fr": "recrutement@acme.fr",
		"mail to: rh@acme.fr please":     "rh@acme.fr",
		"not an address":                 "",
		"a@b":                            "",
		"":                               "",
	}
	for input, want := range cases {
		if got := Email(input); got != want {
			t.Errorf("Email(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestClassifyLocalPart(t *testing.T) {
	cases := map[string]string{
		"recrutement@acme.fr":    "recruitment",
		"jobs-france@acme.fr":    "recruitment",
		"Careers2024@acme.fr":    "recruitment",
		"rh@acme.fr":             "hr",
		"personalabteilung@x.de": "hr",
		"contact@acme.fr":        "general",
		"info@acme.fr":           "general",
		"facturation@acme.fr":    "other",
		"":                       "other",
	}
	for input, want := range cases {
		if got := ClassifyLocalPart(input); got != want {
			t.Errorf("ClassifyLocalPart(%q) = %q, want %q", input, got, want)
		}
	}
}

// Addresses belonging to a named person must be recognised so extraction can drop them.
func TestLooksPersonal(t *testing.T) {
	personal := []string{
		"marie.dupont@acme.fr",
		"j.dupont@acme.fr",
		"marie-claire.dupont@acme.fr",
		"hans.mueller@acme.de",
	}
	for _, address := range personal {
		if !LooksPersonal(address) {
			t.Errorf("LooksPersonal(%q) = false, want true", address)
		}
	}
	functional := []string{
		"recrutement@acme.fr",
		"jobs@acme.fr",
		"contact@acme.fr",
		"rh@acme.fr",
		"info@acme.fr",
		"facturation@acme.fr",
		"",
	}
	for _, address := range functional {
		if LooksPersonal(address) {
			t.Errorf("LooksPersonal(%q) = true, want false", address)
		}
	}
}

func TestIdentityKeys(t *testing.T) {
	if NameIdentity("Renault SAS", "FR") != NameIdentity("  renault  s.a.s. ", "fr") {
		t.Error("the same name in the same country must produce one identity")
	}
	if NameIdentity("Acme", "fr") == NameIdentity("Acme", "de") {
		t.Error("a name must not join companies across countries")
	}
	if DomainIdentity("https://www.acme.fr/x") != "domain:acme.fr" {
		t.Errorf("unexpected domain identity: %q", DomainIdentity("https://www.acme.fr/x"))
	}
	if DomainIdentity("not a url") != "" {
		t.Error("an unreadable domain must yield no identity, not a bad one")
	}
}

func TestCompanyIDIsStableAndDistinct(t *testing.T) {
	id := CompanyID("domain:acme.fr")
	if len(id) != 16 {
		t.Fatalf("company id length = %d, want 16", len(id))
	}
	if id != CompanyID("domain:acme.fr") {
		t.Error("company id is not stable across calls")
	}
	if id == CompanyID("domain:acme.de") {
		t.Error("different identities produced the same id")
	}
}

func TestStableHashSeparatesParts(t *testing.T) {
	if StableHash("ab", "c") == StableHash("a", "bc") {
		t.Error("parts are not separated, so different inputs collide")
	}
}

func TestStripAccents(t *testing.T) {
	if got := StripAccents("Société Générale"); got != "Societe Generale" {
		t.Errorf("StripAccents = %q", got)
	}
}
