-- 0008: Slack questions, users, event de-duplication, alert history, import
-- state and the alert cursor (architecture.md §2, §3.7, §5; spikes/slack.md).
--
-- questions is the one table for every question finbot asks in Slack. A
-- question is found again by (channel_id, message_ts) when a click or thread
-- reply arrives, after a restart too. 'pending' means decided but not yet
-- posted because of the question cap, so it has no channel or ts yet; 'closed'
-- may never have been posted. At most one live (pending or open) question per
-- plan, kind and subject. answered_by is a users.label.
--
-- users maps a Slack user id to who that is; rows are upserted from config.
--
-- slack_events holds the de-duplication key of every handled event or click
-- (event_id, or container.message_ts + action_ts) for 7 days.
--
-- alerts is the alert history: dedupe_key stops an alert being raised twice,
-- and rolled_up marks a non-critical alert held for the monthly report by the
-- alert budget.
--
-- import_state is the last bank-import POST per plan; last_error is redacted.
--
-- alert_cursor is the newest transactions.first_seen_at the anomaly rules have
-- seen, so they see only new rows across restarts.

CREATE TABLE questions (
    id           INTEGER PRIMARY KEY,
    plan_id      TEXT    NOT NULL,
    kind         TEXT    NOT NULL CHECK (kind IN ('categorize', 'dry_run_check')),
    subject_id   TEXT    NOT NULL CHECK (subject_id <> ''),
    channel_id   TEXT,
    message_ts   TEXT,
    options      TEXT    NOT NULL CHECK (json_valid(options) AND json_type(options) = 'array'),
    status       TEXT    NOT NULL CHECK (status IN ('pending', 'open', 'answered', 'closed')),
    answer       TEXT,
    answered_by  TEXT    CHECK (answered_by IS NULL OR answered_by IN ('brendan', 'wife')),
    answered_at  TEXT,
    close_reason TEXT,
    created_at   TEXT    NOT NULL,
    CHECK ((channel_id IS NULL) = (message_ts IS NULL)),
    CHECK (status IN ('pending', 'closed') OR channel_id IS NOT NULL),
    CHECK (status <> 'pending' OR channel_id IS NULL),
    CHECK ((status = 'answered') = (answer IS NOT NULL AND answered_by IS NOT NULL AND answered_at IS NOT NULL)),
    CHECK ((status = 'closed') = (close_reason IS NOT NULL))
) STRICT;

CREATE UNIQUE INDEX questions_message ON questions (channel_id, message_ts);
CREATE INDEX questions_status ON questions (plan_id, status, id);
CREATE INDEX questions_subject ON questions (plan_id, subject_id, id);
CREATE UNIQUE INDEX questions_live ON questions (plan_id, kind, subject_id) WHERE status IN ('pending', 'open');

CREATE TABLE users (
    slack_user_id TEXT NOT NULL PRIMARY KEY CHECK (slack_user_id <> ''),
    label         TEXT NOT NULL UNIQUE CHECK (label IN ('brendan', 'wife')),
    updated_at    TEXT NOT NULL
) STRICT;

CREATE TABLE slack_events (
    dedupe_key TEXT NOT NULL PRIMARY KEY CHECK (dedupe_key <> ''),
    seen_at    TEXT NOT NULL
) STRICT;

CREATE INDEX slack_events_seen ON slack_events (seen_at);

CREATE TABLE alerts (
    id         INTEGER PRIMARY KEY,
    plan_id    TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind <> ''),
    dedupe_key TEXT    NOT NULL CHECK (dedupe_key <> ''),
    critical   INTEGER NOT NULL CHECK (critical IN (0, 1)),
    status     TEXT    NOT NULL CHECK (status IN ('sent', 'rolled_up')),
    body       TEXT    NOT NULL,
    created_at TEXT    NOT NULL
) STRICT;

CREATE UNIQUE INDEX alerts_dedupe ON alerts (plan_id, kind, dedupe_key);
CREATE INDEX alerts_created ON alerts (plan_id, created_at);

CREATE TABLE import_state (
    plan_id         TEXT NOT NULL PRIMARY KEY,
    last_attempt_at TEXT NOT NULL,
    last_ok_at      TEXT,
    last_error      TEXT
) STRICT;

CREATE TABLE alert_cursor (
    plan_id            TEXT NOT NULL PRIMARY KEY,
    last_first_seen_at TEXT NOT NULL
) STRICT;

-- TransactionsFirstSeenAfter reads new rows by first_seen_at.
CREATE INDEX transactions_first_seen ON transactions (plan_id, first_seen_at);
