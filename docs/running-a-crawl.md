# Running a crawl, and where the data goes

This repository is the engine. It holds no dataset, publishes no dataset, and runs no
schedule. A crawl runs in a **private** repository that holds the sources, the workflows
and the data, and which uses this engine as a dependency.

## Why the crawl does not run here

A public repository leaks its outputs three ways, and all three are easy to miss:

| | Who can read it |
|---|---|
| Actions **logs** | Anyone. A crawl that prints company names or contact addresses has published them. |
| Workflow **artifacts** | Anyone with read access to the repository — on a public repository, anyone. |
| **Release assets** | Anyone, permanently, and indexed. |

A workflow that crawled here and attached `companies.json` to a release would be publishing
the dataset, whatever the intention behind it. There was such a workflow in this repository
and it was removed: it created a public release asset on every non-dry run, uploaded the
same files as an artifact, and needed a token with write access to a private repository
stored in a public repository's secrets. A private runner needs none of that — not the
token, not the discipline of keeping output out of the log.

The trade is Actions minutes. A public repository gets them free and unmetered; a private
one on the Free plan gets 2,000 a month. Buying privacy by construction with minutes is the
right way round, because the alternative depends on every future change to a workflow
remembering not to print, upload or release anything.

## What the runner does

The private runner checks this engine out at a pinned commit, builds `eucd`, and runs one
job with its steps in order:

```
eucd config validate                                   # bad config stops the run before any traffic
eucd discover                                          # read each association's member list
eucd enrich --concurrency 8 --limit 0 --deadline 150m   # visit each member's own site
eucd export --format json -o data/companies.json       # the durable state and the deliverable
eucd export --format csv  -o data/companies.csv
```

Three details matter and are easy to get wrong:

- **`--limit 0`.** `enrich` defaults to 50 companies, so a run without this quietly stops
  after fifty and the deadline never bites.
- **The deadline belongs below the job timeout**, and should be derived from it rather than
  written twice. A soft deadline above the hard timeout means the job is killed instead of
  stopping, and the steps that save the results never run.
- **Everything after the crawl runs under `if: always()`.** A validation step that fails the
  run must not also throw away three hours of somebody else's bandwidth.

## State between runs

The committed `data/companies.json` is the durable state, not just the output. `eucd import
dataset` rebuilds the database from it, so the database itself never has to be committed:

```
eucd import dataset data/companies.json
```

That matters for cost, not tidiness. The database is what records `crawl_status` and
`last_checked_at`, and without them every run would re-visit every company it has ever seen.
Keeping the database in `actions/cache` makes a run faster; the import is what makes a cache
miss survivable. Raise the runner's cache retention from the default seven days — a private
repository may keep caches for up to a year — so a quiet fortnight cannot cost a full
re-crawl.

## Adding a source

An association needs one file:

```yaml
slug: de_pharma_deutschland
name: Pharma Deutschland e.V.
country: de
sectors: [pharma]
directory_url: https://www.pharmadeutschland.de/mitglieder/
adapter: link_harvest
access_method: html
limits:
  max_pages: 10
  requests_per_minute: 10
```

`link_harvest` needs no selectors: it reads the member list as the companies the page links
to. It is imprecise by nature — it cannot tell a member from a sponsor — so every record it
produces says so, and a source worth more precision is upgraded to `html_directory` with
the selectors documented in [the adapter protocol](adapter-protocol.md).

Before adding any source, check it may be read: that the member list is published to be
read, that its terms and `robots.txt` permit it, and that no login stands in the way. If one
of those fails, keep the file with `disabled: true` and a `disabled_reason`, so the next
person does not rediscover the same dead end.

The country must exist in `config/countries/` first. That file also carries the contact
policy — including whether a general inbox may be surfaced at all — which is a judgement
to make per country rather than a formality.
