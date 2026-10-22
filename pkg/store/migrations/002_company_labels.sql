-- Judgements about a company, kept apart from facts about it.
--
-- Everything in `companies` and `contacts` was read off a page: a contact marked
-- 'published' means the address appeared, in full, on a page we fetched, and
-- `found_on_url` names it. A label is a different kind of thing. It is an opinion — today,
-- a language model's — about what a company is, and it is right only as often as the
-- thing that produced it.
--
-- They live in their own table for that reason, and the export keeps them under their own
-- key. A consumer reading a company's sectors or contacts is reading evidence; a consumer
-- reading its labels is reading a guess, and must be able to tell which is which without
-- knowing how either was produced. Labels can be deleted wholesale and recomputed; a fact
-- cannot be recomputed, only re-read.

CREATE TABLE company_labels (
    company_id TEXT NOT NULL REFERENCES companies (company_id) ON DELETE CASCADE,
    -- kind is the question that was asked, so a later question does not overwrite an
    -- earlier answer to a different one.
    kind       TEXT NOT NULL,
    label      TEXT NOT NULL,
    -- reason is the one-line explanation that came with the answer. An unexplained
    -- judgement cannot be reviewed, which is the same rule company_rankings follows.
    reason     TEXT NOT NULL DEFAULT '',
    -- source names what produced it — a model id, or a person. Without it there is no way
    -- to find and discard everything one bad source decided.
    source     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (company_id, kind)
);

CREATE INDEX idx_company_labels_label ON company_labels (kind, label);
