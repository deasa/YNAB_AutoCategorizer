-- 0007: the audit log of every YNAB write and the permanent human locks
-- (architecture.md §2, §3.1 step 2; R6b, R7, N9).
--
-- write_log holds one row per write, inserted as an intent before the request
-- goes out, so a crash leaves a row to reconcile. Rows are never deleted. id is
-- global, since a write is reverted by that id alone. txn_id and category_id are
-- NULL for a write that doesn't touch one. before_json and after_json are always
-- valid JSON: a create stores the JSON value null as its before, a delete as its
-- after. Several rows share a request_id when one bulk request carried them.
--
-- human_locks marks a transaction field a person owns: it is never overwritten.

CREATE TABLE write_log (
    id          INTEGER PRIMARY KEY,
    plan_id     TEXT    NOT NULL,
    actor       TEXT    NOT NULL CHECK (actor IN ('system', 'brendan', 'wife')),
    via         TEXT    NOT NULL CHECK (via IN ('scheduler', 'mcp', 'slack')),
    tier        TEXT    NOT NULL CHECK (tier <> ''),
    endpoint    TEXT    NOT NULL CHECK (endpoint <> ''),
    txn_id      TEXT,
    category_id TEXT,
    before_json TEXT    NOT NULL CHECK (json_valid(before_json)),
    after_json  TEXT    NOT NULL CHECK (json_valid(after_json)),
    request_id  TEXT,
    status      TEXT    NOT NULL CHECK (status IN ('intent', 'done', 'failed')),
    error       TEXT,
    created_at  TEXT    NOT NULL,
    reverted_by INTEGER REFERENCES write_log (id)
) STRICT;

CREATE INDEX write_log_txn ON write_log (plan_id, txn_id, id);
CREATE INDEX write_log_status ON write_log (status, id);

CREATE TABLE human_locks (
    plan_id    TEXT NOT NULL,
    txn_id     TEXT NOT NULL,
    field      TEXT NOT NULL CHECK (field <> ''),
    source     TEXT NOT NULL CHECK (source IN ('ynab_edit', 'first_touch', 'slack')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (plan_id, txn_id, field)
) STRICT;
