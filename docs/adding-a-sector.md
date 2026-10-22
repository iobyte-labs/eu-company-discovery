# Adding a sector

Pure configuration, and shared across every country — that is the point of separating the
two axes.

The full procedure is in the agent skill, which is the single source of truth:

**[`.claude/skills/add-country-or-sector/SKILL.md`](../.claude/skills/add-country-or-sector/SKILL.md)**

In short:

1. Create `config/sectors/<slug>.yaml`.
2. `keywords` identify the sector; `activities` describe what companies there *do* and are
   more discriminating; `synonyms` map a canonical term to its other spellings.
3. Put every language in the one file — German, French and English together.
4. `negative_keywords` do most of the quality work: without them an industry search returns
   its recruitment agencies, trade press and distributors. A term may not be both a keyword
   and a negative keyword.
5. `eucd config validate`, then `eucd companies list --sector <slug>` and add negative
   keywords until the wrong companies are gone. That loop is the work.
