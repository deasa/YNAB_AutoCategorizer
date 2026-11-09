-- 0006: the categorizer's decision log (architecture.md §2, §3.2 step 6).
--
-- Every outcome is a row, and rows are never rewritten except to set
-- superseded_by. Every key carries plan_id, because a Fresh Start brings new
-- plan and transaction ids. id counts up per plan, so it is insertion order.
--
-- category_name, confidence and runner_up are NULL when no category was chosen
-- (a skip). model, policy_hash and candidates_hash are NOT NULL so "is this
-- decision current" is plain equality; a decision without one stores ''.

CREATE TABLE decisions (
    plan_id         TEXT    NOT NULL,
    id              INTEGER NOT NULL,
    txn_id          TEXT    NOT NULL,
    category_id     TEXT,
    category_name   TEXT,
    confidence      REAL,
    runner_up       TEXT,
    method          TEXT    NOT NULL CHECK (method IN ('rule', 'model')),
    model           TEXT    NOT NULL,
    policy_hash     TEXT    NOT NULL,
    candidates_hash TEXT    NOT NULL,
    mode            TEXT    NOT NULL CHECK (mode IN ('dry_run', 'auto')),
    outcome         TEXT    NOT NULL CHECK (outcome IN ('would_apply', 'applied', 'asked', 'skipped')),
    second_opinion  TEXT    CHECK (second_opinion IS NULL OR json_valid(second_opinion)),
    features        TEXT    NOT NULL CHECK (json_valid(features)),
    created_at      TEXT    NOT NULL,
    superseded_by   INTEGER,
    PRIMARY KEY (plan_id, id),
    FOREIGN KEY (plan_id, superseded_by) REFERENCES decisions (plan_id, id)
        DEFERRABLE INITIALLY DEFERRED
) STRICT;

CREATE INDEX decisions_txn ON decisions (plan_id, txn_id, id);

-- A transaction has at most one current decision.
CREATE UNIQUE INDEX decisions_current ON decisions (plan_id, txn_id) WHERE superseded_by IS NULL;
