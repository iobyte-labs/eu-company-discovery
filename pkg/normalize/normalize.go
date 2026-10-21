// Package normalize turns the many spellings of a company, URL or address into one.
//
// Deduplication lives or dies here, so everything in this package is pure, deterministic
// and offline. Two rules shape it:
//
// Never merge on resemblance. Identity keys are exact after normalisation. "Renault SA"
// and "Renault SAS" stay two companies; deciding they are one is a judgement the engine is
// not entitled to make. Only a shared registrable domain, or an identical normalised name
// within one country, joins two records.
//
// Normalise only what is notation. Case, accents, punctuation and whitespace carry no
// meaning in a company name, so they go. The legal form (SAS, GmbH, BV) does carry
// meaning, so it stays.
package normalize

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/net/idna"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// multiLabelSuffixes are public suffixes of more than one label, needed to find the
// registrable domain of hosts like "acme.co.uk".
//
// This is deliberately a curated list rather than the full Public Suffix List. The PSL is a
// ~15,000-line moving target that would have to be downloaded and kept fresh, and the cost
// of being wrong here is only a canonical domain one label too long — a missed join, never
// a false one. Add entries rather than reaching for the network.
var multiLabelSuffixes = map[string]bool{
	// United Kingdom / Ireland
	"co.uk": true, "org.uk": true, "ltd.uk": true, "plc.uk": true, "net.uk": true,
	"ac.uk": true, "gov.uk": true, "me.uk": true, "co.ie": true,
	// France and territories
	"com.fr": true, "asso.fr": true, "nom.fr": true, "tm.fr": true, "avocat.fr": true,
	"gouv.fr": true,
	// German-speaking
	"com.de": true, "co.at": true, "or.at": true, "ac.at": true, "gv.at": true,
	"co.ch": true, "com.ch": true,
	// Southern Europe
	"com.es": true, "org.es": true, "nom.es": true, "edu.es": true, "gob.es": true,
	"com.it": true, "edu.it": true, "gov.it": true,
	"com.pt": true, "edu.pt": true, "gov.pt": true,
	"com.gr": true, "edu.gr": true, "gov.gr": true, "net.gr": true, "org.gr": true,
	"com.cy": true, "com.mt": true, "org.mt": true,
	// Benelux / Nordics / Baltics
	"com.be": true, "co.nl": true, "com.lu": true,
	"com.pl": true, "com.ee": true, "com.hr": true, "com.ro": true, "com.ua": true,
	"co.no": true, "priv.no": true, "com.se": true, "org.se": true, "pp.se": true,
	"co.dk": true, "co.is": true, "com.is": true,
	// Central / Eastern Europe
	"co.hu": true, "com.hu": true, "co.rs": true, "com.si": true, "com.sk": true,
	"com.cz": true, "com.bg": true, "com.tr": true, "org.tr": true, "gen.tr": true,
	// Non-EU but common for EU company sites
	"com.au": true, "co.nz": true, "com.br": true, "co.jp": true, "com.cn": true,
	"co.in": true, "com.mx": true,
	// gTLD second levels and hosts seen under company sites
	"co.com": true, "eu.com": true, "uk.com": true, "us.com": true,
	"web.app": true, "firebaseapp.com": true, "github.io": true, "gitlab.io": true,
	"pages.dev": true, "netlify.app": true, "vercel.app": true, "wixsite.com": true,
	"squarespace.com": true, "myshopify.com": true, "weebly.com": true,
	"wordpress.com": true, "blogspot.com": true, "sharepoint.com": true,
	"azurewebsites.net": true,
}

// trackingParams identify a visit rather than a page, so two links to the same page
// normalise to one URL.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "dclid": true, "msclkid": true, "twclid": true,
	"igshid": true, "mc_cid": true, "mc_eid": true, "yclid": true, "ref": true,
	"ref_src": true, "referrer": true, "source": true, "s_kwcid": true, "wickedid": true,
}

var trackingPrefixes = []string{"utm_", "pk_", "mtm_", "piwik_", "ga_", "_hs"}

// Functional local parts. Used to classify an address that was found published — never to
// construct one.
var (
	recruitmentLocals = set(
		"recrutement", "recrutements", "recruitment", "recruiting", "recruiter",
		"jobs", "job", "career", "careers", "carriere", "carrieres", "emploi", "emplois",
		"candidature", "candidatures", "bewerbung", "bewerbungen", "karriere",
		"talent", "talents", "talentacquisition", "apply", "application", "applications",
		"stage", "stages", "alternance", "lavoro", "trabajo", "vacature", "vacatures",
		"werkenbij", "rekrytering", "hiring",
	)
	hrLocals = set(
		"rh", "drh", "hr", "humanresources", "ressourceshumaines",
		"personal", "personalabteilung", "people", "peopleops", "hrbp",
		"risorseumane", "recursoshumanos", "personeelszaken",
	)
	generalLocals = set(
		"contact", "contacts", "info", "information", "hello", "bonjour", "kontakt",
		"office", "mail", "email", "enquiries", "enquiry", "inquiries", "general",
		"accueil", "reception", "secretariat", "administration", "admin",
	)
)

func set(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

var (
	whitespaceRe  = regexp.MustCompile(`\s+`)
	nonNameCharRe = regexp.MustCompile(`[^0-9a-z]+`)
	multiSlashRe  = regexp.MustCompile(`/{2,}`)
	allDigitDotRe = regexp.MustCompile(`^[0-9.]+$`)
	emailRe       = regexp.MustCompile(
		`[A-Za-z0-9._%+\-]+@[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?` +
			`(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?)+`)
	atObfuscationRe  = regexp.MustCompile(`(?i)\s*[\(\[\{]\s*(?:at|arobase|chez)\s*[\)\]\}]\s*`)
	dotObfuscationRe = regexp.MustCompile(`(?i)\s*[\(\[\{]\s*(?:dot|point|punkt)\s*[\)\]\}]\s*`)
	spacedAtRe       = regexp.MustCompile(`\s*@\s*`)
	splitNonAlnumRe  = regexp.MustCompile(`[^0-9a-z]+`)
	// Local parts run words and numbers together ("careers2024", "hr2"). Splitting at the
	// letter/digit boundary is what lets "careers2024@" be recognised as a careers address.
	letterDigitRe = regexp.MustCompile(`([a-z])([0-9])`)
	digitLetterRe = regexp.MustCompile(`([0-9])([a-z])`)
)

// accentStripper decomposes, drops combining marks, then recomposes.
var accentStripper = transform.Chain(
	norm.NFD,
	runes.Remove(runes.In(unicode.Mn)),
	norm.NFC,
)

// nonDecomposable are Latin letters that Unicode decomposition cannot help with, because
// they are letters in their own right rather than a base plus an accent. Without these,
// "Ørsted" would normalise to "rsted" and every Nordic, Icelandic and Baltic company name
// would dedup against the wrong thing.
var nonDecomposable = strings.NewReplacer(
	"ø", "o", "Ø", "O",
	"æ", "ae", "Æ", "Ae",
	"œ", "oe", "Œ", "Oe",
	"ß", "ss", "ẞ", "Ss",
	"đ", "d", "Đ", "D",
	"ð", "d", "Ð", "D",
	"þ", "th", "Þ", "Th",
	"ł", "l", "Ł", "L",
	"ħ", "h", "Ħ", "H",
	"ŧ", "t", "Ŧ", "T",
	"ŋ", "n", "Ŋ", "N",
	"ı", "i", "İ", "I",
	"ĸ", "k",
)

// ---------------------------------------------------------------- strings

// StripAccents turns "Société" into "Societe" and "Ørsted" into "Orsted".
func StripAccents(text string) string {
	out, _, err := transform.String(accentStripper, nonDecomposable.Replace(text))
	if err != nil {
		return text
	}
	return out
}

// CollapseWhitespace trims and squeezes every run of whitespace to one space.
func CollapseWhitespace(text string) string {
	return strings.TrimSpace(whitespaceRe.ReplaceAllString(text, " "))
}

// deletedInName are notation *inside* a token: removing them rather than splitting on them
// is what turns "S.A.S." into "sas" instead of "s a s".
var deletedInName = strings.NewReplacer(
	".", "", "'", "", "’", "", "ʼ", "", "`", "",
)

// CompanyName reduces a name to its notation-free form:
//
//	"  Établissements Dupont & Fils S.A.S. " -> "etablissements dupont fils sas"
//
// Not an identity key on its own, because a name is only unique within a country. Use
// [NameIdentity] for that.
func CompanyName(name string) string {
	folded := strings.ToLower(StripAccents(name))
	folded = deletedInName.Replace(folded)
	return CollapseWhitespace(nonNameCharRe.ReplaceAllString(folded, " "))
}

// DisplayName is the name as it should be shown: whitespace tidied, nothing else touched.
func DisplayName(name string) string { return CollapseWhitespace(name) }

// ---------------------------------------------------------------- hosts

// Host lowercases a hostname, strips a trailing dot and IDNA-encodes it. It returns "" for
// anything that is not a usable hostname, so callers get a falsy value rather than an error
// on junk input.
func Host(host string) string {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "."))
	if host == "" {
		return ""
	}
	encoded, err := idna.Lookup.ToASCII(host)
	if err != nil {
		// Hosts with underscores fail IDNA's stricter rules but still resolve in
		// practice. Anything non-ASCII that will not encode is not a hostname.
		if isASCII(host) {
			return host
		}
		return ""
	}
	return encoded
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func looksLikeIP(host string) bool {
	if strings.Contains(host, ":") {
		return true
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return allDigitDotRe.MatchString(host)
}

// RegistrableDomain returns the domain someone could register:
// "shop.renault.co.uk" -> "renault.co.uk".
//
// Unknown suffixes fall back to the last two labels, which is correct for every
// single-label TLD. IP addresses have no registrable domain and return "".
func RegistrableDomain(host string) string {
	host = Host(host)
	if host == "" || looksLikeIP(host) {
		return ""
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return ""
	}
	// A 3-label suffix would need a 4th label to be registrable, hence len > take.
	for _, take := range []int{3, 2} {
		if len(labels) > take && multiLabelSuffixes[strings.Join(labels[len(labels)-take:], ".")] {
			return strings.Join(labels[len(labels)-take-1:], ".")
		}
	}
	return strings.Join(labels[len(labels)-2:], ".")
}

// CanonicalDomain returns the registrable domain of a URL or a bare host, or "".
//
// This is the engine's strongest identity signal: two records that share a canonical domain
// are the same company.
func CanonicalDomain(urlOrHost string) string {
	candidate := strings.TrimSpace(urlOrHost)
	if candidate == "" || !isWebScheme(schemeOf(candidate)) {
		return ""
	}
	if !strings.Contains(candidate, "//") {
		// A bare host, or a URL written without a scheme ("www.acme.fr/careers").
		candidate = "//" + strings.TrimLeft(candidate, "/")
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Hostname() == "" {
		return RegistrableDomain(strings.SplitN(strings.TrimPrefix(candidate, "//"), "/", 2)[0])
	}
	return RegistrableDomain(parsed.Hostname())
}

// ---------------------------------------------------------------- urls

func isTrackingParam(key string) bool {
	low := strings.ToLower(key)
	if trackingParams[low] {
		return true
	}
	for _, prefix := range trackingPrefixes {
		if strings.HasPrefix(low, prefix) {
			return true
		}
	}
	return false
}

var defaultPorts = map[string]string{"http": "80", "https": "443"}

var schemeRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.\-]*):`)

// schemeOf returns the lowercase scheme of raw, or "" when it carries none.
//
// This has to run before any parse. "mailto:info@acme.fr" has no "//", so prefixing it
// with "https://" yields "https://mailto:info@acme.fr", which Go parses happily as userinfo
// "mailto:info" on host "acme.fr" — turning a mail link into a plausible-looking website.
// Deciding the scheme first is what stops that.
func schemeOf(raw string) string {
	match := schemeRe.FindStringSubmatch(raw)
	if match == nil {
		return ""
	}
	// "acme.fr:8080/x" is a host and port, not a scheme.
	if rest := raw[len(match[0]):]; rest != "" && rest[0] >= '0' && rest[0] <= '9' {
		return ""
	}
	return strings.ToLower(match[1])
}

// isWebScheme reports whether a scheme may be fetched: present and http(s), or absent.
func isWebScheme(scheme string) bool {
	return scheme == "" || scheme == "http" || scheme == "https"
}

// URL produces one canonical spelling per page, so the same link is never fetched twice.
//
// It lowercases the scheme and host, drops a default port, drops the fragment, drops
// tracking parameters, sorts what survives, and removes a bare trailing slash. It returns
// "" when the input has no usable host or is not http(s) — which is also the first line of
// defence for the fetcher, since a URL that will not normalise is never requested.
func URL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Decide the scheme before parsing: see schemeOf. A non-web scheme is rejected here
	// rather than being mangled into something fetchable.
	declared := schemeOf(raw)
	if !isWebScheme(declared) {
		return ""
	}
	if declared == "" {
		// "acme.fr/x" would otherwise parse as a path with no host.
		raw = "https://" + strings.TrimLeft(raw, "/")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := Host(parsed.Hostname())
	if host == "" {
		return ""
	}

	out := &url.URL{Scheme: scheme, Host: host}
	if port := parsed.Port(); port != "" && port != defaultPorts[scheme] {
		out.Host = host + ":" + port
	}

	path := multiSlashRe.ReplaceAllString(parsed.EscapedPath(), "/")
	if path == "" {
		path = "/"
	}
	if path != "/" {
		path = strings.TrimRight(path, "/")
	}
	if path != "/" {
		out.RawPath = path
		unescaped, err := url.PathUnescape(path)
		if err != nil {
			unescaped = path
		}
		out.Path = unescaped
	}

	if parsed.RawQuery != "" {
		kept := make([]string, 0, 4)
		for _, pair := range strings.Split(parsed.RawQuery, "&") {
			if pair == "" {
				continue
			}
			key, _, _ := strings.Cut(pair, "=")
			if !isTrackingParam(key) {
				kept = append(kept, pair)
			}
		}
		sort.Strings(kept)
		out.RawQuery = strings.Join(kept, "&")
	}
	return out.String()
}

// SameSite reports whether two URLs share a registrable domain — one site, for the purpose
// of crawl limits and "is this really the company's own page".
func SameSite(a, b string) bool {
	da, db := CanonicalDomain(a), CanonicalDomain(b)
	return da != "" && da == db
}

// ---------------------------------------------------------------- emails

// Email lowercases and trims an address and undoes the usual anti-scraper obfuscation
// ("info (at) acme [dot] fr"). It returns "" if the input holds no address.
//
// Undoing obfuscation is reading what the page says, not guessing: an address still only
// gets recorded because a page published it.
func Email(address string) string {
	text := strings.TrimSpace(strings.Trim(strings.TrimSpace(address), "<>"))
	if text == "" {
		return ""
	}
	text = strings.NewReplacer("​", "", " ", " ").Replace(text)
	text = atObfuscationRe.ReplaceAllString(text, "@")
	text = dotObfuscationRe.ReplaceAllString(text, ".")
	// Close up whitespace around the "@" only. Stripping every space instead would glue
	// the following word onto the domain: "rh@acme.fr please" -> "rh@acme.frplease".
	text = spacedAtRe.ReplaceAllString(CollapseWhitespace(text), "@")

	match := emailRe.FindString(text)
	if match == "" {
		return ""
	}
	at := strings.LastIndex(match, "@")
	local, domain := match[:at], Host(match[at+1:])
	if domain == "" || !strings.Contains(domain, ".") {
		return ""
	}
	return strings.ToLower(local) + "@" + domain
}

// EmailLocal returns the part before the "@", normalised.
func EmailLocal(address string) string {
	local, _, _ := strings.Cut(Email(address), "@")
	return local
}

// EmailDomain returns the part after the "@", normalised.
func EmailDomain(address string) string {
	_, domain, _ := strings.Cut(Email(address), "@")
	return domain
}

// localTokens turns "jobs-france2024" into {jobs, france, 2024, jobsfrance2024}, so that
// both the separated words and the run-together spelling can be matched.
func localTokens(local string) map[string]bool {
	parts := splitNonEmpty(strings.ToLower(local))
	tokens := make(map[string]bool, len(parts)+1)
	for _, part := range parts {
		tokens[part] = true
	}
	tokens[strings.Join(parts, "")] = true
	return tokens
}

// splitNonEmpty splits on anything that is not a lowercase letter or digit, and also at
// letter/digit boundaries.
func splitNonEmpty(s string) []string {
	s = letterDigitRe.ReplaceAllString(s, "$1 $2")
	s = digitLetterRe.ReplaceAllString(s, "$1 $2")
	out := make([]string, 0, 4)
	for _, part := range splitNonAlnumRe.Split(s, -1) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func anyIn(tokens, vocabulary map[string]bool) bool {
	for token := range tokens {
		if vocabulary[token] {
			return true
		}
	}
	return false
}

// ClassifyLocalPart reads an address's purpose from its local part: "recruitment", "hr",
// "general" or "other".
//
// A classification, not a verification. The caller still records which page the address was
// found on and whether that page published it.
func ClassifyLocalPart(address string) string {
	local := EmailLocal(address)
	if local == "" {
		return "other"
	}
	tokens := localTokens(local)
	switch {
	case anyIn(tokens, recruitmentLocals):
		return "recruitment"
	case anyIn(tokens, hrLocals):
		return "hr"
	case anyIn(tokens, generalLocals):
		return "general"
	default:
		return "other"
	}
}

// LooksPersonal reports whether a local part looks like a named individual rather than a
// function.
//
// It exists to *exclude*. The engine never records or targets an individual's address, so
// anything matching this is dropped at extraction time. It errs towards calling an address
// personal, because wrongly keeping one is the costly mistake.
func LooksPersonal(address string) bool {
	local := EmailLocal(address)
	if local == "" {
		return false
	}
	if ClassifyLocalPart(address) != "other" {
		return false
	}
	parts := splitNonEmpty(strings.ToLower(local))
	if len(parts) == 2 && len(parts[0]) == 1 && isAlpha(parts[1]) {
		return true // "j.dupont"
	}
	if len(parts) >= 2 && isAlpha(parts[0]) && len(parts[0]) >= 2 &&
		isAlpha(parts[1]) && len(parts[1]) >= 2 {
		return true // "marie.dupont", "marie-claire.dupont"
	}
	return false
}

func isAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- identity

// NameIdentity builds "name:fr:renault sas". A name is only unique inside one country, so
// the country is part of the key.
func NameIdentity(name, country string) string {
	return "name:" + strings.ToLower(strings.TrimSpace(country)) + ":" + CompanyName(name)
}

// DomainIdentity builds "domain:renault.fr", or "" when no registrable domain can be read.
func DomainIdentity(urlOrHost string) string {
	domain := CanonicalDomain(urlOrHost)
	if domain == "" {
		return ""
	}
	return "domain:" + domain
}

// CompanyID derives a stable, opaque id from one identity key.
//
// A company keeps the id of the identity it was *first* seen under. Learning its website
// later adds a second identity row pointing at the same id rather than renaming it; that
// join happens in the store, not here, which is what makes repeated runs idempotent.
func CompanyID(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])[:16]
}

// StableHash is a content hash for caching. Order matters; parts are separated so that
// ("ab","c") and ("a","bc") hash differently.
func StableHash(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		digest.Write([]byte(part))
		digest.Write([]byte{0x1f})
	}
	return hex.EncodeToString(digest.Sum(nil))
}
