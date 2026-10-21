---
name: add-country-or-sector
description: Extend eu-company-discovery to a new EU country, industry sector or professional specialization through configuration alone. Use when asked to support another country, add an industry or sector, add keywords or local-language terms for matching, or when checking that adding one requires no engine change.
---

# Adding a country or a sector

Both are pure configuration. If a change to `pkg/` seems necessary to add a country or a
sector, that is a design regression — say so rather than making the change, because the
whole architecture rests on this property.

## A new country

Create `config/countries/<cc>.yaml`, where `<cc>` is the lowercase ISO 3166-1 alpha-2 code.

```yaml
code: de
name: Germany
languages: [de, en]              # at least one; drives letter language and matching

regions:                         # the first-level administrative division
  - code: by
    name: Bayern
    aliases: [Bavaria, Munich, München]    # every spelling a directory might print

contact_policy:
  prefer_recruitment_contacts: true
  allow_general_fallback: true   # may a contact@/info@ inbox be surfaced at all?
  require_careers_page_check: true

compliance_notes: |
  What was established about recruitment and data-protection practice here, and what is
  still open. Not legal advice, not machine-read. See docs/compliance.md.
```

### The parts that need actual research

**`aliases`** carries the real weight. Directories print `Ile-de-France`,
`Île-de-France`, `IDF` and `Paris Region` for one place. Accents and case are handled by
normalisation; genuinely different names are not. An incomplete alias list produces
companies with no region, which then score badly on geography for no good reason.

**`contact_policy`** is per-country because recruitment practice is not uniform across the
EU. What is ordinary in one member state is unwelcome in another. Set
`allow_general_fallback: false` where approaching a general inbox is not acceptable; a
company publishing only `info@` is then reported as having no usable contact rather than
being queued.

**`compliance_notes`** is prose for a human. Write what you actually verified, and write
the open questions as open questions. Do not write reassurance you have not earned. The
France file is the model: an "Established" list and an "Open questions" list.

### Verify

```bash
eucd config validate
eucd associations list --all      # a country with no sources is useless
```

Then add at least one source for it — see the `add-association` skill. A country file with
no association is configuration with nothing behind it.

## A new sector

Create `config/sectors/<slug>.yaml`.

```yaml
slug: aerospace
name: Aerospace and Defence
keywords:                        # the terms that identify the sector
  - aerospace
  - aéronautique
  - Luftfahrt
synonyms:                        # canonical term -> other spellings, in any language
  quality assurance: [assurance qualité, QA, Qualitätssicherung]
activities:                      # what companies here DO; matches descriptions, not names
  - usinage de précision
  - traitement de surface
negative_keywords:               # "probably NOT in this sector despite a keyword hit"
  - agence de recrutement
  - trade fair
```

### The parts that need actual thought

**Put every language in one file.** A sector file is shared across countries — that is the
point of separating sector from country. Write German, French and English terms together.
Matching is case- and accent-insensitive, so write terms naturally.

**`negative_keywords` do most of the quality work.** Without them a search for an industry
returns its recruitment agencies, its trade press, its conference organisers and its
distributors. They are the cheapest precision available. A term may not appear in both
`keywords` and `negative_keywords` — `config validate` rejects that.

**`activities` beat `keywords` for precision.** A company's own description of what it does
("emboutissage", "cleaning validation") is far more discriminating than its industry label.

### Verify

```bash
eucd config validate
eucd discover --association <slug>
eucd companies list --sector <slug>
```

Check the result for companies that should not be there, and add `negative_keywords` until
they are gone. That loop is the work.

## What you may not do

- Do not put country-specific terms in a sector file, or sector terms in a country file.
  The two axes are independent; crossing them means a second country needs the sector file
  edited, which defeats the design.
- Do not add a region alias you have not seen a source actually print. Invented aliases
  cause wrong region assignment, which is worse than none.
- Do not copy another country's `compliance_notes`. Employment and privacy practice differ
  per member state; copied reassurance is the one kind of documentation worse than none.
- Do not touch `pkg/` for either task. If you believe you must, stop and explain why.

## Checklist

- [ ] `eucd config validate` passes
- [ ] Region aliases cover the spellings real sources use
- [ ] `compliance_notes` separates what is established from what is open
- [ ] The sector has `negative_keywords`
- [ ] No term is both a keyword and a negative keyword
- [ ] At least one association exists for a new country
- [ ] Nothing under `pkg/` changed
