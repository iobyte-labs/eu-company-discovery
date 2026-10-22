---
name: add-profile
description: Create or tune a ranking profile in eu-company-discovery, including one derived from a CV. Use when asked to add a profile, decide which companies are worth applying to, tune why the wrong companies rank highly, set skill weights or a min_score, or turn an uploaded CV into a profile the engine can rank against.
---

# Adding and tuning a ranking profile

A profile is what companies are scored against. It lives in `config/profiles/<slug>.yaml`,
or — when derived from a CV — in a private file handed over by path.

Read [`docs/ranking.md`](../../../docs/ranking.md) first for how scoring works. This is the
procedure.

## The rule that governs everything here

**A profile holds preferences only: skills, sectors, places.** No name, no CV text, no
address, no contact details, no employer history. This repository is public. `model.Profile`
has nowhere to put personal data, and nothing should be added that does.

A CV is personal data. A profile *derived* from one is not, provided the derivation keeps
only the preferences.

## Writing one

```yaml
slug: de-pharma-qa                 # must match the filename stem
headline: Pharmacovigilance, QA/QC and audit roles in German pharma

countries: [de]                    # must exist in config/countries/
regions: [by, bw]                  # region codes from those countries; a preference, not a filter
sectors: [pharma]                  # must exist in config/sectors/
languages: [de, en]

must_have: [pharma]                # a company matching none of these cannot rank at all
exclude:                           # any hit vetoes outright, score 0
  - personalvermittlung
  - zeitarbeit

skills:
  - term: pharmakovigilanz
    weight: 10
    aliases: [pharmacovigilance, drug safety, arzneimittelsicherheit]
  - term: lohnabfuellung           # negative weights subtract
    weight: -3

size_buckets: [small, medium]
min_score: 40.0
```

### What actually needs thought

**Weights are relative, not absolute.** The skills factor scores the share of your total
positive weight that matched, so doubling every weight changes nothing. What matters is
which terms outweigh which.

**Aliases carry more weight than the term.** Companies write "drug safety", "PV",
"Arzneimittelsicherheit" and "pharmacovigilance" for one thing. A term with no aliases
matches one spelling out of four. Write them in every language the sector's countries use.

**`exclude` is the strongest tool you have.** One hit vetoes the company outright at score
0 and nothing else is computed. Use it for the categories that are never right — agencies,
publishers, dealerships — and prefer it to fighting them with weights.

**`must_have` is the bluntest.** Every term is a company you will never see at any score.
Use one, or none.

**`min_score` is a review threshold, not a storage one.** Scores below it are still stored,
which is what lets you look at what was rejected.

## Deriving one from a CV

The derivation happens **privately**, never in this repository. Produce a JSON file with the
same shape and hand it over by path:

```bash
eucd rank --profile-file /private/path/derived-from-cv.json
```

It is validated exactly as a committed profile is — same countries, same sectors, same
strict decoding — so a generated file with a misspelled key fails loudly rather than scoring
against a preference nobody expressed.

When generating one:

- Keep skills, sectors, languages and places. Drop everything else the CV contains.
- Weight by depth of experience, not by how recent the job was.
- Put the CV's own terminology in `aliases` — that is what company pages will say.
- Never emit a name, an employer, a date or a contact detail. There is no field for one,
  and a validator will not catch what a reviewer would.
- If a model does the extraction, the CV goes to it only with the person's explicit
  consent. Their own words are the input; the output is checked by them before it is used.

## Verifying

```bash
eucd config validate                                      # or the file is checked on load
eucd rank --profile <slug> --dry-run                      # score, write nothing
eucd rank --profile <slug>
eucd companies top --profile <slug> --limit 20 --explain
```

**Read the top twenty.** That is the test. A profile that validates and scores nothing
useful has passed every automated check.

## Tuning: what to change when the wrong companies rank highly

| Symptom | Fix | Not |
|---|---|---|
| Agencies, publishers, law firms near the top | the sector's `negative_keywords`, or `exclude` | lowering skill weights |
| Everything scores the same, flat list | companies have no description — check first | re-weighting |
| A company you know is right scores low | check it has a description and a contact | raising its terms' weights |
| Nothing reaches `min_score` | lower the threshold, or the data is thin | widening `must_have` |

**Check the data before changing the profile.** The most common cause of a flat ranking is
that companies have no text to match against:

```bash
eucd export --format json | grep -c '"description": "'
```

A company that never says what it does cannot be scored on what it does. That is a gap in
the crawl, not in the profile — run `eucd enrich` rather than editing weights.

## Hard rules

- Never put personal data in a profile, even in a private one handed over by path.
- Never add a term to `exclude` that is also a scored skill — exclude always wins, so the
  skill becomes dead code. `config validate` rejects this.
- Never put a term in both `must_have` and `exclude`; nothing could ever score.
- Never give a skill `weight: 0`. It is never counted; remove it or weight it.
- Do not edit `pkg/rank` to make a profile behave. If a preference cannot be expressed in
  configuration, say so rather than special-casing it in the scorer.
