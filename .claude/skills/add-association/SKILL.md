---
name: add-association
description: Add a new association, member directory or crawlable website as a discovery source in eu-company-discovery. Use when the admin supplies a directory URL to crawl, asks to add a source, association, chamber of commerce, industry federation or member list, or when a configured source has stopped returning rows and its selectors need repair.
---

# Adding a discovery source

A source is a YAML file in `config/associations/`. Most need **no code at all**. Prefer
configuration over a new adapter every time — a code adapter is a maintenance burden and a
selector change is not.

## Before writing anything: check the source may be read

Do this first, and record the answer in the file. Skipping it is how a project acquires a
source it is not entitled to crawl.

1. Fetch `<site>/robots.txt` and check the directory path is not disallowed.
2. Look for a login wall, a CAPTCHA, or terms that forbid automated access.
3. Prefer, in this order: an official API → a published dataset (CSV/JSON/XML download) →
   public HTML → a hand-curated list.

**If the source refuses automated access, stop.** Do not work around it. Write the file
anyway with `disabled: true` and a `disabled_reason` saying exactly what was found. A
documented dead end stops the next person rediscovering it; a deleted one does not.

## Decide the adapter

| Situation | `adapter` | `access_method` |
|---|---|---|
| Repeating HTML rows on a listing page | `html_directory` | `html` |
| Documented JSON/XML API | `api_directory` | `api` |
| Downloadable CSV/JSON/XML file | `dataset` | `dataset` |
| A list you typed in by hand | `seed_list` | `manual` |
| Anything genuinely odd | a new adapter — see `docs/adapter-protocol.md` | |

Run `eucd adapters` to see what this build actually has. An adapter not in that list cannot
be used by an *enabled* source.

## Write the file

`config/associations/<country>_<sector>_<shortname>.yaml`. The slug must match the filename
stem and be lowercase letters, digits, `-` or `_`.

```yaml
slug: fr_automotive_fiev
name: FIEV — French automotive equipment suppliers
country: fr                      # must exist in config/countries/
sectors: [automotive]            # must exist in config/sectors/
directory_url: https://example.org/members
adapter: html_directory
access_method: html

limits:
  max_pages: 25                  # a cap, not a target
  requests_per_minute: 10        # be a polite guest
  timeout: 20s
  max_response_bytes: 2097152

settings:
  row_selector: "ul.member-list > li.member"
  fields:
    name: "h3.member-name"
    website:
      selector: "a.member-site"
      attribute: href
    region: "span.member-region"
    description: "p.member-summary"
  pagination:
    mode: next_link
    next_selector: "a[rel=next]"
  fail_on_empty_page: true       # a page with no rows is a broken selector
```

Notes that matter:

- **Keep `fail_on_empty_page: true`.** Without it, a changed selector makes the run
  "succeed" with zero companies and nobody notices for weeks.
- **One registrable domain per company.** Entries whose websites are subdomains of one
  domain are deduplicated into a single company. See `docs/knowledge-concepts.md` §2.
- Set `max_pages` even when the directory looks small. It is a brake, not an estimate.

## Verify, in this order

```bash
eucd config validate                                 # exit 2 on any problem
eucd discover --association <slug> --dry-run         # reads the source, writes nothing
eucd discover --association <slug>
eucd discover --association <slug>                   # must report 0 new
eucd companies list --country <cc>
```

The second `discover` reporting **0 new** is the check that matters: it proves the source
deduplicates against itself. If it reports new companies on a second run, the records are
missing a stable identity — usually a website the adapter failed to extract.

Read the output for `warning:`, `BLOCKED` and `identity collision:` lines. None of them are
decoration.

## When a working source stops returning rows

The site changed its markup. Do not loosen the selectors until something matches — that
silently harvests the wrong elements.

1. Fetch one listing page and save it to `testdata/<slug>/page1.html`.
2. Write a fixture test that asserts the expected row count and one known company.
3. Fix the selectors against the fixture.
4. Commit the fixture with the fix, so the next change is caught by a test rather than by
   someone noticing an empty run.

## Hard rules

- Never add credentials, cookies, session tokens or an `Authorization` header to a source
  config to get past a gate.
- Never raise `requests_per_minute` above ~20, and lower it if the source is slow.
- Never commit a real member list scraped into `seed_list` in the public repository. Curated
  placeholders are fine; a harvested dataset masquerading as curated config is not.
- `disabled: true` always needs a `disabled_reason`. `config validate` enforces it.
