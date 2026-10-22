# Adding a country

Pure configuration. If a change under `pkg/` seems necessary, that is a design regression —
say so rather than making the change.

The full procedure, including the parts that need real research (region aliases,
`contact_policy`, `compliance_notes`), is in the agent skill, which is the single source of
truth so the two cannot drift:

**[`.claude/skills/add-country-or-sector/SKILL.md`](../.claude/skills/add-country-or-sector/SKILL.md)**

In short:

1. Create `config/countries/<cc>.yaml` — lowercase ISO 3166-1 alpha-2 code.
2. List `languages` (at least one) and `regions` with every alias a directory might print.
3. Set `contact_policy` for that country's recruitment practice.
4. Write `compliance_notes`: what you established, and what is still open.
5. `eucd config validate`
6. Add at least one association — a country with no sources is configuration with nothing
   behind it. See [adding-an-association.md](adding-an-association.md).
