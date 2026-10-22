# Adding an association or crawlable website

A source is one YAML file in `config/associations/`. Most need **no code at all**.

The full procedure — including the permission check that must come first, the declarative
selector settings, and what to do when a working source stops returning rows — is in the
agent skill, which is the single source of truth:

**[`.claude/skills/add-association/SKILL.md`](../.claude/skills/add-association/SKILL.md)**

In short:

1. **Check the source may be read**: `robots.txt`, login walls, CAPTCHAs, terms. Prefer an
   official API → a published dataset → public HTML → a curated list. If it refuses, write
   the file with `disabled: true` and a `disabled_reason` and stop — do not work around it.
2. Pick the adapter (`eucd adapters` shows what this build has).
3. Write `config/associations/<country>_<sector>_<name>.yaml` with `limits` and, for
   `html_directory`, the selector `settings`. Keep `fail_on_empty_page: true`.
4. Verify:
   ```bash
   eucd config validate
   eucd discover --association <slug> --dry-run
   eucd discover --association <slug>
   eucd discover --association <slug>   # must report 0 new
   ```

The second `discover` reporting **0 new** is the check that matters — it proves the source
deduplicates against itself.

See also [the adapter protocol](adapter-protocol.md) for sources that need code, and
[compliance](compliance.md) for the permission question.
