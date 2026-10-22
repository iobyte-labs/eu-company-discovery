# eu-company-discovery

A sector-neutral engine for discovering companies through industry association directories,
resolving their official websites, extracting the recruitment contacts those companies
published, and ranking them against a configurable profile.

**This repository holds no personal data.** No CV, no profile, no collected addresses for a
campaign, no letters, no send history, no credentials. It writes no applications and sends
no mail — that belongs to a private consumer which reads this engine's versioned export.
See [the public/private boundary](docs/knowledge-concepts.md#9-the-publicprivate-boundary).

Written in Go: the work is fetching thousands of pages under per-domain rate limits and
parsing HTML, which is I/O-bound concurrency with a parsing hot path. With a pure-Go SQLite
driver it builds to one static binary with no libc dependency, runs from a ~15 MB
distroless image, and holds ~25 MB resident.

## Install

```bash
go install github.com/iobyte-labs/eu-company-discovery/cmd/eucd@latest
```

Or build from source (Go 1.25+):

```bash
git clone https://github.com/iobyte-labs/eu-company-discovery
cd eu-company-discovery
make build        # -> bin/eucd
make check        # format, vet, test
```

Or run the container:

```bash
docker build -t eucd .
docker run --rm -v "$PWD/data:/data" -v "$PWD/config:/config:ro" eucd config validate
```

## Try it

The committed configuration has one France/automotive source using the `seed_list`
adapter, which fetches nothing — so this works offline:

```bash
make demo
```

That runs discovery twice and lists the result. The second run creating nothing is the
point: discovery is idempotent.

```bash
eucd config validate                     # every problem at once, exit 2 if any
eucd adapters                            # what this build can read
eucd associations list --all             # configured sources, including disabled ones
eucd discover --dry-run                  # read the sources, write nothing
eucd discover                            # record what was found
eucd enrich --dry-run --limit 10         # visit company sites, report what would be stored
eucd enrich --deadline 45m               # record published contacts and careers pages
eucd companies list --country fr
eucd export --format json -o companies.json
eucd db status
```

## How it is extended

Everything below is configuration or a separate package. The engine does not change.

| To add | Do this | Code? |
|---|---|---|
| A country | `config/countries/<cc>.yaml` | No |
| A sector | `config/sectors/<slug>.yaml` | No |
| A member directory | `config/associations/<slug>.yaml` with CSS selectors | No |
| A bespoke source | A Go package calling `directory.Register` from `init` | In its own repo |
| A source in another language | The `exec` adapter — any binary speaking the JSON contract | In any language |

See [adding a country or sector](docs/adding-a-country.md),
[adding an association](docs/adding-an-association.md) and
[the adapter protocol](docs/adapter-protocol.md).

### Agent skills

`.claude/skills/` holds reusable skills for coding agents working in this repository:
`add-association`, `add-country-or-sector` and `safe-crawling`. They encode the rules that
are expensive to rediscover — check a source may be read before crawling it, never
construct an address, a blocked source is reported rather than worked around.

## How it works

Four ideas carry most of the design. [`docs/knowledge-concepts.md`](docs/knowledge-concepts.md)
explains all twelve.

**Provenance.** Every company knows which directory listed it and when; every contact knows
the page it was read from. A contact claiming to be published with no page named is rejected
outright — the claim and its evidence are inseparable.

**Identity is exact, never approximate.** A company is keyed on its registrable domain, or
on its normalised name *within one country*. Normalisation removes notation (case, accents,
punctuation) but not meaning, so `Renault SA` and `Renault SAS` stay two companies. There is
no fuzzy matching anywhere: a wrong merge destroys information irrecoverably, while a missed
merge leaves two records a human can join later.

**Idempotency comes from constraints.** Identities, sources and contacts have uniqueness
constraints, so a run that crashes halfway and is restarted cannot double anything. In
particular several contacts never mean several applications.

**Safety is structural.** Every URL came from untrusted input. `pkg/fetch.Guard` refuses
non-web schemes and ports, local hostnames, cloud metadata endpoints, private and reserved
address ranges, IPv4-mapped spellings of those, and numeric shorthands like `127.1` — then
dials the address it approved, which closes the DNS-rebinding window that a
check-then-fetch design leaves open.

## Layout

```
cmd/eucd/            the binary
internal/cli/        cobra command tree
pkg/model/           shared data types
pkg/normalize/       identity, dedup keys, URL/name/email normalisation
pkg/config/          country, sector and association configuration
pkg/store/           SQLite persistence, migrations, dedup
pkg/fetch/           HTTP safe to point at an untrusted URL
pkg/directory/       the adapter contract and registry
pkg/export/          the versioned data contract
pkg/errors/          deliberate failures and their exit codes
config/              example countries, sectors and associations
docs/                concepts, protocols, compliance
```

## Status

Phase 1 (foundation) is complete and tested: packaging, configuration, data model, schema
and migrations, identity and deduplication, the SSRF guard, the adapter registry with the
`seed_list` adapter, the versioned export, and the CLI.

Phase 2 (fetching and extraction) is complete and tested: the fetcher — guarded redirects,
per-domain rate limiting, a response cap, robots.txt, an on-disk page cache — the
`html_directory` and `exec` adapters, contact and careers-page extraction, and `eucd
enrich`. Every test runs offline against recorded fixtures and local servers.

Not yet built: the `api_directory` and `dataset` adapters, deterministic ranking, and the
optional OpenRouter provider. `eucd adapters` always tells the truth about what a build can
actually do.

## Exit codes

`0` success · `2` configuration · `3` unknown adapter · `4` adapter failure · `5` unsafe
URL · `6` source blocked · `7` migration · `8` provider · `64` usage · `130` interrupted.
A command never exits 0 after failing.

## Compliance

This engine is for individual, relevant employment applications — not bulk unsolicited
marketing. A published address does not by itself authorise automated outreach. Read
[`docs/compliance.md`](docs/compliance.md) and the per-country `compliance_notes` before
contacting anyone, and [`docs/byok-sending.md`](docs/byok-sending.md) before wiring mail
credentials to anything.

Sources that block automated access are reported and left alone. Nothing here circumvents
a login, a CAPTCHA or `robots.txt`.

## License

MIT. See [LICENSE](LICENSE).
