# Ranking

Ranking answers one question: of the companies the engine found, which are worth applying
to? It is deterministic, entirely offline, and every point it awards comes with a reason.

```bash
eucd rank --profile de-pharma-qa
eucd companies top --profile de-pharma-qa --explain
```

No network, no model, no API key. An LLM is never consulted for a score.

## Why a score has to be explainable

A ranking nobody can audit is a ranking nobody should act on. Every sub-factor emits a
`model.Reason` carrying a code, a human-readable detail and its points — and **the points
always sum to the score**. That equality is a test, not an aspiration: where a cap or a
clamp bites, an adjustment reason carries the difference so the arithmetic still closes.

```
  1.   59.9  Agon Pharma GmbH                       de/—  agon.de
             +8.4  skill.match         "analytik" as "analysis" (weight 6.0) in description
             +7.0  skill.match         "regulatory affairs" (weight 5.0) in description
            +16.3  sector.match        pharma: 4 vocabulary term(s) in the company's own text
             +4.1  sector.declared     listed under sector "pharma"
            +10.2  geo.country         de is a preferred country
             +3.1  contact.general     published general address
             +5.1  evidence.quality    data quality is high
             +3.1  evidence.freshness  last checked 0 day(s) ago, within 30 days
             +2.6  language.assumed    no language recorded; de is spoken in de
```

The store refuses to save a ranking with no reasons. `reasons_json` is `NOT NULL`, but
`"[]"` would satisfy that, so the rule is enforced where it can actually be reached.

## The factors

| Factor | Weight | What it reads |
|---|---|---|
| `skills` | 40 | The profile's weighted terms, with aliases, against the company's own text |
| `sector` | 20 | The sector config's vocabulary; at most 4 of the 20 for a directory's filing |
| `geo.country` | 10 | The company's country against the profile's |
| `geo.region` | 5 | The region, resolved through its name and aliases |
| `contact` | 10 | Recruitment > HR > general, published beating unverified |
| `evidence.quality` | 5 | `data_quality` |
| `evidence.freshness` | 3 | How long since the site was visited |
| `language` | 5 | The company's languages, or its country's as a weaker fallback |
| `size` | 2 | The size bucket, when known |

**`Exclude` vetoes.** One hit anywhere returns score 0 with a single `exclude.veto` reason,
and nothing else is computed — even for a company that would otherwise have scored 95.

**`MustHave` gates.** A missing term returns 0 with `must_have.missing`, naming *every*
missing term rather than the first.

Vetoed and gated companies are still stored and still reported. "Where did the company I
expected go?" is the first question anyone asks of a ranking.

## Scores are 0–100 and mean the same thing in every profile

Each profile is scored over only the factors it actually expresses, then rescaled to 100.
Without that, a profile stating no language preference could never exceed 95, and
`min_score` would silently mean something different in every file.

The cost: a reason's point value is only interpretable with the profile in hand. That is why
the first reason is always `score.basis`, naming the active factors and carrying zero points
so the sum still closes.

## Three deliberate refusals

**Membership is not evidence.** The ranker never reads `Company.Sources`. A company listed
by three associations scores exactly what one listed by none does. Being in a directory is
how a company was *found*, not evidence that it fits. A directory's filing is worth at most
4 points of 100, and a sector's negative keywords can take that factor negative — so a trade
publisher in a pharma directory lands below the threshold rather than above it.

**No fuzzy matching.** Terms match on word boundaries after accent-stripping and
lowercasing. `"auto"` does not match `"automotive"`. Multi-word phrases work because both
sides collapse separators to single spaces.

**Matching is binary per term, never a count.** The store's merge keeps the longer
description, so descriptions grow monotonically across visits. Counting occurrences would
make scores drift upward over time for no reason anybody asked for.

## What the skills factor reads, and why it matters

Skills are matched against the company's own words: its name, description, keywords and
sectors. **Never its website, domain or careers URL** — a company at `recrutement-auto.fr`
would otherwise match a recruitment term because of its domain name, which is evidence of
nothing.

This is why `enrich` captures a description (see [`extract.Describe`](../pkg/extract/extract.go)).
Before it did, all 391 companies in the first real crawl had none, and the largest factor
scored zero for everybody. If `companies top` looks flat, check how many companies have a
description:

```bash
eucd export --format json | grep -c '"description": "'
```

A company that never says what it does cannot be scored on what it does. That is a gap in
the data, not in the ranker.

## Two factors that are inert until something fills them

`size` scores an unknown bucket as **neutral**, not as a mismatch — nothing in the engine
learns a company's size yet, and the gap is ours rather than theirs. `language` falls back
to the country's languages at **half credit**, because nothing populates `Company.Languages`
during discovery and an assumption is weaker evidence than a fact.

Both are weighted low enough that their inertness costs little. Remove the fallbacks once
something fills the fields.

## Writing a profile

A profile holds **preferences only** — skills, sectors, places. No CV, no name, no contact
details. That is not an oversight: this repository is public, and a profile that needed
redacting could not live in it.

See [`config/profiles/de-pharma-qa.yaml`](../config/profiles/de-pharma-qa.yaml) for a worked
example and [the add-profile skill](../.claude/skills/add-profile/SKILL.md) for the
procedure.

### A profile derived from a CV

A CV *is* personal data, so a profile derived from one is written somewhere private and
handed to the engine by path:

```bash
eucd rank --profile-file /private/path/derived-from-cv.json
```

It is validated exactly as a committed profile is, against the same countries and sectors,
with the same strict decoding — a generated file with a misspelled key would otherwise score
against a preference nobody expressed. What arrives carries preferences only, because
`model.Profile` has nowhere to put a name or an address.

The derivation itself belongs to the private consumer. The engine neither reads a CV nor
wants to.

## Tuning

The loop is short and the output is the feedback:

```bash
eucd rank --profile <slug>
eucd companies top --profile <slug> --limit 20 --explain
```

Read the top twenty. If a recruitment agency, a trade publisher or a law firm ranks highly,
the fix is almost always the sector's `negative_keywords` or the profile's `exclude` — not
the weights. Scores below `min_score` are stored precisely so you can look at what was
rejected and why.

## Exit codes

`2` configuration or an unusable profile file · `64` no profile given, or both `--profile`
and `--profile-file`. A ranking run that stores nothing still exits 0 when there was simply
nothing to rank; that is a result, not a failure.
