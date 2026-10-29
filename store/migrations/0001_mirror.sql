-- 0001: YNAB mirror tables (architecture.md §2).
--
-- Every table is keyed by plan_id. YNAB deletions arrive as tombstones and are
-- kept as rows with deleted = 1. Money is YNAB milliunits (INTEGER). Dates are
-- 'YYYY-MM-DD' TEXT; timestamps are RFC 3339 UTC TEXT.
--
-- Foreign keys are declared only for parent/child composition that YNAB
-- guarantees (transaction->subtransaction, month->month category,
-- scheduled->scheduled sub). They are DEFERRABLE INITIALLY DEFERRED
-- so a sync batch can write rows in any order inside one transaction.
-- Cross-entity references (account_id, payee_id, category_id, ...) are not
-- FKs: a split parent's category_id is YNAB's internal "Split" sentinel, which
-- is not in categories, and full exports omit deleted accounts, payees and
-- category groups (while including deleted categories), so category ->
-- category_group is not an FK either.

CREATE TABLE accounts (
    plan_id                TEXT    NOT NULL,
    id                     TEXT    NOT NULL,
    name                   TEXT    NOT NULL,
    type                   TEXT    NOT NULL,
    on_budget              INTEGER NOT NULL,
    closed                 INTEGER NOT NULL,
    note                   TEXT,
    balance                INTEGER NOT NULL,
    cleared_balance        INTEGER NOT NULL,
    uncleared_balance      INTEGER NOT NULL,
    transfer_payee_id      TEXT,
    direct_import_linked   INTEGER NOT NULL,
    direct_import_in_error INTEGER NOT NULL,
    last_reconciled_at     TEXT,
    debt_original_balance  INTEGER,
    -- Loan terms: JSON objects of {"YYYY-MM-DD": milliunits}, NULL when empty.
    debt_interest_rates    TEXT,
    debt_minimum_payments  TEXT,
    debt_escrow_amounts    TEXT,
    deleted                INTEGER NOT NULL DEFAULT 0,
    -- Local only; never written by sync.
    tax_bucket             TEXT,
    PRIMARY KEY (plan_id, id)
) STRICT;

CREATE TABLE category_groups (
    plan_id  TEXT    NOT NULL,
    id       TEXT    NOT NULL,
    name     TEXT    NOT NULL,
    hidden   INTEGER NOT NULL,
    internal INTEGER NOT NULL,
    deleted  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, id)
) STRICT;

CREATE TABLE categories (
    plan_id                    TEXT    NOT NULL,
    id                         TEXT    NOT NULL,
    category_group_id          TEXT    NOT NULL,
    name                       TEXT    NOT NULL,
    hidden                     INTEGER NOT NULL,
    internal                   INTEGER NOT NULL,
    original_category_group_id TEXT,
    note                       TEXT,
    budgeted                   INTEGER NOT NULL,
    activity                   INTEGER NOT NULL,
    balance                    INTEGER NOT NULL,
    goal_type                  TEXT,
    goal_needs_whole_amount    INTEGER,
    goal_day                   INTEGER,
    goal_cadence               INTEGER,
    goal_cadence_frequency     INTEGER,
    goal_creation_month        TEXT,
    goal_target                INTEGER,
    goal_target_month          TEXT,
    goal_target_date           TEXT,
    goal_percentage_complete   INTEGER,
    goal_months_to_budget      INTEGER,
    goal_under_funded          INTEGER,
    goal_overall_funded        INTEGER,
    goal_overall_left          INTEGER,
    goal_snoozed_at            TEXT,
    deleted                    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, id)
) STRICT;
CREATE INDEX categories_group ON categories (plan_id, category_group_id);

CREATE TABLE payees (
    plan_id             TEXT    NOT NULL,
    id                  TEXT    NOT NULL,
    name                TEXT    NOT NULL,
    transfer_account_id TEXT,
    deleted             INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, id)
) STRICT;

CREATE TABLE transactions (
    plan_id                    TEXT    NOT NULL,
    id                         TEXT    NOT NULL,
    date                       TEXT    NOT NULL,
    amount                     INTEGER NOT NULL,
    memo                       TEXT,
    cleared                    TEXT    NOT NULL,
    approved                   INTEGER NOT NULL,
    flag_color                 TEXT,
    flag_name                  TEXT,
    account_id                 TEXT    NOT NULL,
    payee_id                   TEXT,
    -- For a split parent this is YNAB's "Split" sentinel id (not in categories).
    category_id                TEXT,
    transfer_account_id        TEXT,
    transfer_transaction_id    TEXT,
    matched_transaction_id     TEXT,
    import_id                  TEXT,
    import_payee_name          TEXT,
    import_payee_name_original TEXT,
    debt_transaction_type      TEXT,
    deleted                    INTEGER NOT NULL DEFAULT 0,
    -- Local: when this mirror first stored the row, reset when import_id first
    -- appears (a manual entry matched to a bank import). Import freshness, F6.
    first_seen_at              TEXT    NOT NULL,
    PRIMARY KEY (plan_id, id)
) STRICT;
CREATE INDEX transactions_account_date ON transactions (plan_id, account_id, date);
CREATE INDEX transactions_payee ON transactions (plan_id, payee_id);
CREATE INDEX transactions_category ON transactions (plan_id, category_id);

CREATE TABLE subtransactions (
    plan_id                 TEXT    NOT NULL,
    id                      TEXT    NOT NULL,
    transaction_id          TEXT    NOT NULL,
    amount                  INTEGER NOT NULL,
    memo                    TEXT,
    payee_id                TEXT,
    category_id             TEXT,
    transfer_account_id     TEXT,
    transfer_transaction_id TEXT,
    deleted                 INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, id),
    FOREIGN KEY (plan_id, transaction_id) REFERENCES transactions (plan_id, id)
        DEFERRABLE INITIALLY DEFERRED
) STRICT;
CREATE INDEX subtransactions_parent ON subtransactions (plan_id, transaction_id);

CREATE TABLE months (
    plan_id        TEXT    NOT NULL,
    month          TEXT    NOT NULL, -- first of month, 'YYYY-MM-01'
    note           TEXT,
    income         INTEGER NOT NULL,
    budgeted       INTEGER NOT NULL,
    activity       INTEGER NOT NULL,
    to_be_budgeted INTEGER NOT NULL,
    age_of_money   INTEGER,
    deleted        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, month)
) STRICT;

CREATE TABLE month_categories (
    plan_id                  TEXT    NOT NULL,
    month                    TEXT    NOT NULL,
    category_id              TEXT    NOT NULL,
    budgeted                 INTEGER NOT NULL,
    activity                 INTEGER NOT NULL,
    balance                  INTEGER NOT NULL,
    goal_target              INTEGER,
    goal_under_funded        INTEGER,
    goal_overall_funded      INTEGER,
    goal_overall_left        INTEGER,
    goal_percentage_complete INTEGER,
    deleted                  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, month, category_id),
    FOREIGN KEY (plan_id, month) REFERENCES months (plan_id, month)
        DEFERRABLE INITIALLY DEFERRED
) STRICT;

CREATE TABLE scheduled_transactions (
    plan_id             TEXT    NOT NULL,
    id                  TEXT    NOT NULL,
    date_first          TEXT    NOT NULL,
    date_next           TEXT    NOT NULL,
    frequency           TEXT    NOT NULL,
    amount              INTEGER NOT NULL,
    memo                TEXT,
    flag_color          TEXT,
    flag_name           TEXT,
    account_id          TEXT    NOT NULL,
    payee_id            TEXT,
    category_id         TEXT,
    transfer_account_id TEXT,
    deleted             INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, id)
) STRICT;

CREATE TABLE scheduled_subtransactions (
    plan_id                  TEXT    NOT NULL,
    id                       TEXT    NOT NULL,
    scheduled_transaction_id TEXT    NOT NULL,
    amount                   INTEGER NOT NULL,
    memo                     TEXT,
    payee_id                 TEXT,
    category_id              TEXT,
    transfer_account_id      TEXT,
    deleted                  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (plan_id, id),
    FOREIGN KEY (plan_id, scheduled_transaction_id) REFERENCES scheduled_transactions (plan_id, id)
        DEFERRABLE INITIALLY DEFERRED
) STRICT;

CREATE TABLE sync_state (
    plan_id          TEXT    NOT NULL PRIMARY KEY,
    server_knowledge INTEGER NOT NULL,
    synced_at        TEXT    NOT NULL
) STRICT;
