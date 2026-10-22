# The data contract

The versioned export is the only seam between this public engine and any consumer. A
consumer must be able to read it without this repository's source, database or git history.

## The envelope

```json
{
  "schema_version": "eucd.companies/1",
  "generated_at": "2026-10-21T23:02:18.586973Z",
  "generator": "eucd 0.1.0-dev",
  "filter": { "country": "fr", "with_contact": "true" },
  "count": 4,
  "companies": [ ... ]
}
```

| Field | Why it is there |
|---|---|
| `schema_version` | A consumer that silently accepts an unknown version would act on fields it has misunderstood. `export.Read` refuses one. |
| `generated_at` | How stale the data is. |
| `generator` | Traces a surprising export back to a build. |
| `filter` | A partial export must never be mistaken for the whole database. |
| `count` | Checkable against the payload length. |

`companies` is sorted by `company_id`, so two exports of the same data are byte-identical
and diffable.

## Versioning

Versions are `<name>/<major>`. A reader accepts **its own major version only**.

- **Adding an optional field** is not a breaking change. Readers ignore unknown fields, and
  `export.Read` does not reject them.
- **Removing a field, renaming one, or changing what one means** is breaking. Bump
  `model.ExportSchemaVersion`.

## A company

```json
{
  "company_id": "4a921531576e456a",
  "name": "Exemple Emboutissage SAS",
  "normalized_name": "exemple emboutissage sas",
  "country": "fr",
  "region": "ara",
  "website": "https://exemple-emboutissage.example.org",
  "canonical_domain": "example.org",
  "description": "Metal stamping and sub-assembly.",
  "sectors": ["automotive"],
  "careers_url": "https://exemple-emboutissage.example.org/carrieres",
  "contacts": [
    {
      "address": "recrutement@exemple-emboutissage.example.org",
      "kind": "recruitment",
      "verification": "published",
      "found_on_url": "https://exemple-emboutissage.example.org/carrieres",
      "found_at": "2026-10-21T23:02:18Z"
    }
  ],
  "sources": [
    {
      "association_slug": "fr_automotive_seed",
      "source_url": "https://example.org/member-list",
      "raw_name": "Exemple Emboutissage SAS",
      "retrieved_at": "2026-10-21T23:02:18Z"
    }
  ],
  "data_quality": "low",
  "crawl_status": "not_attempted",
  "first_seen_at": "2026-10-21T23:02:18Z",
  "updated_at": "2026-10-21T23:02:18Z"
}
```

### Fields a consumer must respect

**`company_id`** is stable. It is derived from the identity the company was *first* seen
under and never changes, so it is safe to use as a foreign key. Learning a company's
website later adds an identity pointing at the same id rather than renaming it.

**`sources`** is never empty. A company with no provenance is a bug; the CI acceptance job
checks for it.

**`contacts[].verification`** is `published`, `inferred` or `unverified`. The engine only
ever writes `published`, and a `published` contact always names `found_on_url` — the claim
and its evidence are inseparable. Treat `inferred` and `unverified` as needing a human
decision.

**`contacts[].kind`** is `recruitment`, `hr`, `general` or `other`. It says what an inbox
appears to be *for*, not that anyone may be written to. A `general` inbox (`contact@`,
`info@`) is a fallback: a company publishing only that has not invited applications to it.

**No contact is ever an individual's address.** The engine refuses them at extraction and
again in the store. A consumer should not need to filter, but checking costs nothing.

**`crawl_status`** of `blocked` means the source refuses automated access. It is reported,
never worked around, and a consumer should not retry it.

## What is deliberately absent

No profile, no CV, no match score against a person, no draft, no approval state, no send
history, no suppression list, no credentials. Those belong to the private consumer, which
defines its own tables under its own migration namespace against the same SQLite file —
see `store.MigrationSet`.

## CSV

`eucd export --format csv` is a flattened view for spreadsheets and eyeballing. It **cannot
represent more than one contact or source per company**: it carries the best contact plus
`contact_count` and `source_count`. JSON is the contract; do not build a consumer on the
CSV.

## Reading it

```go
envelope, err := export.Read(file)
if errors.Is(err, export.ErrSchemaMismatch) {
    // Regenerate with a matching engine, or upgrade the consumer. Do not guess.
}
```

In any language: check `schema_version` before trusting anything, and fail loudly if it is
not the major version you were written against.
