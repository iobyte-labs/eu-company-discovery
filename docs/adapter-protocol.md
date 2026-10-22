# The adapter protocol

Three ways to add a source, in order of preference. The first needs no code; the second
keeps code out of this repository; the third keeps it out of this language.

Whichever route you take, an adapter's job is narrow: produce `model.DirectoryRecord`
values with their provenance filled in. It does not normalise names, resolve identities,
deduplicate, score, or decide what a company is. The engine does that once, for every
adapter, so that one identity and dedup policy applies everywhere — see
[`knowledge-concepts.md`](knowledge-concepts.md) §2.

---

## What every adapter owes the caller

| Obligation | Why |
|---|---|
| Fetch only through the `directory.Fetcher` you were given | A client built elsewhere bypasses the SSRF guard, the rate limit, the response cap, robots and the cache at once |
| Return partial results rather than discarding them | A page that failed halfway is still worth what it read |
| Report a refusal in `Result.Blocked`, never work around it | A login wall or a robots rule is an answer |
| Set `Result.Truncated` when a limit stopped you | A short result must never look like a complete one |
| Add a `Result.Warning` for every row you skipped | A silently dropped row is a source quietly rotting |
| Return an error only when *nothing* could be read | That is the one case the caller cannot summarise honestly itself |

`Result.PagesFetched` counts requests actually made, so a cached re-run reports zero
rather than claiming work it did not do.

---

## 1. Configuration only — `html_directory`

Most public member directories are one repeating block per member. That needs no code at
all, in any language: a row selector, a selector per field, and a pagination rule in the
association's YAML file.

```yaml
adapter: html_directory
access_method: html
directory_url: https://assoc.example.org/members
limits:
  max_pages: 25
  requests_per_minute: 10
  timeout: 20s
  max_response_bytes: 2097152
settings:
  row_selector: "ul.member-list > li.member"
  fields:
    name: "h3.member-name"          # a bare string selects the element's text
    website:
      selector: "a.member-site"     # a mapping reads an attribute instead
      attribute: href
    region: "span.member-region"
    description: "p.member-summary"
    membership: "span.member-class" # an unrecognised name is kept in Extra
  pagination:
    mode: next_link
    next_selector: "a[rel=next]"
  fail_on_empty_page: true
```

`config/associations/fr_pharma_directory.yaml` is a worked example, kept disabled.

**Fields.** `name` is required. `name`, `website`, `region`, `country` and `description`
map onto the record; any other name is kept in `Extra`, because the engine cannot know in
advance what will turn out to be worth having. A relative `website` is resolved against the
page it was read from; one that is not an `http(s)` URL is dropped with a warning rather
than stored as if it were evidence.

**Pagination.** `next_link` follows a link and is the mode to prefer: it cannot overshoot
the end of the directory, it stops when the link is absent, and it refuses to follow a link
that leaves the directory's own site. `page_param` appends an incrementing query parameter
and therefore requires `max`, because nothing on the page says where the directory ends. A
link pointing back into the chain is detected and stops the run. `max_pages` always
applies, defaulting to 25.

**`fail_on_empty_page`** defaults to true, and it is the setting that matters most. A
listing page that yields no rows means the site changed its markup; a run that reported
success with zero companies would hide that for weeks. Set it to false only for a source
whose last page is genuinely empty — and then an empty page ends pagination *with a
warning*, because opting out of the failure must not mean opting out of being told.

---

## 2. A compile-time adapter, in its own repository

A separate Go module implements `directory.Adapter` and calls `directory.Register` from an
`init` function:

```go
package acmedirectory

import "github.com/iobyte-labs/eu-company-discovery/pkg/directory"

func init() { directory.Register("acme_directory", func() directory.Adapter { return adapter{} }) }
```

A small `main` that imports both packages gets a binary with the new adapter in it, and
`eucd adapters` lists it. The adapter keeps its own tests, CI and release cycle; this engine
stays unchanged. Registering a name twice panics: two adapters competing for one name would
make `eucd discover` behave differently depending on import order.

---

## 3. An external adapter, in any language — `exec`

The `exec` adapter runs a program and reads records from its stdout.

### The contract

**stdin** receives one JSON object:

```json
{
  "protocol": "eucd.exec/1",
  "association": {
    "slug": "fr_registry_exec",
    "country": "fr",
    "sectors": ["pharma"],
    "directory_url": "https://registry.example.org/members",
    "adapter": "exec",
    "access_method": "api",
    "settings": {"registry_endpoint": "…", "page_size": 100},
    "limits": {"max_records": 2000}
  },
  "limits": {"max_records": 2000, "timeout": 120000000000}
}
```

The association carries its own `settings`, so an external adapter is configured exactly as
a built-in one is. Honour `limits`: they are what the operator agreed to do to somebody
else's server.

**stdout** is newline-delimited JSON, one record per line, using the field names of
`model.DirectoryRecord`:

```json
{"raw_name":"Exemple Externe SAS","website":"https://exemple-externe.example.org","source_url":"https://registry.example.org/members","retrieved_at":"2026-03-01T09:00:00Z","region":"idf","description":"…"}
```

`raw_name` and `source_url` are required; `country`, `source_url` and `retrieved_at` are
filled in from the association when omitted. Blank lines are ignored. An unknown field is
an error for that line, not a field quietly dropped — the adapter and the engine disagreeing
about the protocol is worth hearing about.

**stderr** is for diagnostics. It is read, bounded, and included in the error or warning the
engine reports.

**Exit status.** Non-zero with no records is a failure. Non-zero after some records is
reported as a warning, with `Truncated` set: partial success is reported, never discarded
and never mistaken for a complete read. Zero with no usable records is *also* a failure,
whatever the status said.

### Configuration

```yaml
adapter: exec
access_method: api              # whatever the external adapter actually does
directory_url: https://registry.example.org/members
limits:
  max_records: 2000
  timeout: 2m
settings:
  command: /opt/eucd-adapters/registry-reader
  args: ["--country", "fr"]
  max_output_bytes: 4194304
  registry_endpoint: https://registry.example.org/api/v2/members
```

### What bounds it

An escape hatch that is not bounded is a hole.

- **The operator authorises the program, not the configuration.** The command must be on the
  allowlist in `EUCD_EXEC_ALLOW` (a `:`-separated list of absolute paths to programs, or to
  directories whose programs are allowed). Configuration in this repository decides what to
  *read*, never what to *execute*: otherwise a pull request to a public repository could run
  a program on the crawler while looking like it was adding a directory. Both the configured
  path and what it resolves to must be allowed, so a symlink inside an allowed directory is
  not a way out of it.
- **No shell.** `exec.CommandContext` with an argv slice. Nothing in configuration is ever
  parsed as shell syntax, so no quoting mistake can turn an argument into a command.
- **A scrubbed environment.** Only `PATH`, `HOME`, `TMPDIR`, the locale and the TLS
  certificate paths are passed through. A third-party program has no business reading an API
  key that happens to be exported.
- **A timeout** (`settings.timeout`, or the association's), after which the child is killed
  rather than waited for — including a child that spawned a grandchild holding its stdout.
- **An output cap** (`settings.max_output_bytes`), after which the pipe closes and the run
  reports itself truncated. A runaway adapter cannot fill the engine's memory.

### Writing one

Any language. The whole of a minimal adapter:

```python
#!/usr/bin/env python3
import json, sys
request = json.load(sys.stdin)
settings = request["association"]["settings"]
for member in fetch_members(settings["registry_endpoint"]):   # your code
    print(json.dumps({
        "raw_name": member["name"],
        "website": member.get("url", ""),
        "source_url": request["association"]["directory_url"],
    }))
```

Then authorise it (`export EUCD_EXEC_ALLOW=/opt/eucd-adapters/registry-reader`) and point an
association at it. `config/associations/fr_registry_exec.yaml` is a worked example, kept
disabled.

---

## Before you point any adapter at a real source

From [`.claude/skills/safe-crawling`](../.claude/skills/safe-crawling/SKILL.md), which is
the operative rulebook:

- Check the source publishes a member list intended to be read, that its terms and
  `robots.txt` permit automated access, and that no login or CAPTCHA stands in the way.
- If one of those fails, mark the association `disabled: true` with a `disabled_reason` and
  move on. Keep it in the repository: a dead end that is deleted gets rediscovered every few
  months.
- Never construct an address, never solve a CAPTCHA, never rotate a user-agent to look like
  someone else, and never ignore `robots.txt` because the data is public.
