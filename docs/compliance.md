# Compliance

**Not legal advice.** This records what the project assumes, what it enforces in code, and
what remains a human decision. Read it, and the per-country `compliance_notes` in
`config/countries/*.yaml`, before contacting anyone.

## What this engine is for

Individual, relevant employment applications — a person looking for work, approaching
companies where their experience plausibly fits. It is **not** built for bulk unsolicited
marketing, and the differences are deliberate: conservative volume caps, multi-minute gaps,
a preference for addresses a company published specifically for applications, and a
suppression list that outlives the database.

**A published address does not by itself authorise automated outreach.** That a mailbox is
visible on a website is not consent to receive anything.

## What the code enforces, so it cannot be forgotten

| Rule | Where |
|---|---|
| An individual's address is never stored | `normalize.LooksPersonal`, refused at extraction *and* in `upsertContactTx` |
| No address is ever constructed, only read | No pattern-generation code exists; `verification: inferred` is never written by the engine |
| A `published` contact must name its page | `model.Contact.Validate` |
| Robots, logins and CAPTCHAs are respected | `directory.Blocked` is a result; no circumvention code exists |
| Requests cannot reach private networks | `fetch.Guard` |
| Several contacts never mean several applications | `UNIQUE (company_id, address)` |

Two layers on the personal-address rule on purpose: it is a promise the project makes, not
a step someone might forget.

## GDPR, as it applies here

**Functional versus personal.** `recrutement@acme.fr` names a function and is business
contact information. `marie.dupont@acme.fr` identifies a person and is personal data. The
engine stores the first and refuses the second.

**Lawful basis.** For an individual sending their own job application, the normal basis is
their own legitimate interest in seeking work. That basis does **not** stretch to bulk
commercial messaging. Volume, relevance and the ability to stop on request are what
separate the two, which is why the caps and the suppression list are not optional features.

**Transparency.** An application should say plainly who is writing and why. Hiding that it
is an unsolicited approach, or implying a vacancy that does not exist, is both dishonest
and a compliance problem.

**Right to object.** An opt-out must be honoured permanently and must survive a database
rebuild, a re-import and a version migration. The suppression list is append-only and
checked immediately before each message, not only when a batch was assembled.

**Data minimisation and retention.** Keep what is needed to decide whether to apply, and
set an explicit retention period for contacts of companies you decided against. Retention
belongs to the private consumer.

## What is not uniform across the EU

Employment, privacy and recruitment practice differ by member state. What is ordinary in one
is unwelcome in another. That is why `contact_policy` and `compliance_notes` are
per-country configuration rather than a single global setting, and why copying one
country's notes to another is the one kind of documentation worse than none.

`allow_general_fallback: false` makes a company that publishes only `info@` report as having
no usable contact, rather than being queued.

## Per-source permission

Whether a given directory may be read automatically is a **per-source** question. Check it
before adding a source, and record the answer in the association's file:

1. `robots.txt` for the directory path.
2. A login wall, a CAPTCHA, or terms forbidding automated access.
3. Prefer an official API → a published dataset → public HTML → a hand-curated list.

If a source refuses, mark it `disabled: true` with a `disabled_reason` and keep the file. A
documented dead end stops the next person rediscovering it.

## Before any campaign

- [ ] Each enabled source has been checked and the finding recorded
- [ ] The country's `compliance_notes` separate what is established from what is open
- [ ] Volume caps and warm-up are set conservatively
- [ ] The suppression list is imported, append-only, and checked at send time
- [ ] Predecessor send history and opt-outs are imported (see knowledge-concepts §10)
- [ ] Letters claim nothing the CV does not support
- [ ] An unsolicited application is clearly distinguished from a reply to a vacancy
- [ ] A retention period is set for companies decided against
- [ ] Credentials are handled per `byok-sending.md` — never a workflow_dispatch input
