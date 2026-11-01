-- 0003: the one Slack status message finbot keeps editing, plus the last
-- backup's outcome shown in it, so neither is lost on a restart. At most one
-- row. The message and the backup are written separately, so either pair can
-- still be NULL. post_attempt_at is when finbot last tried to post a new
-- message; it caps re-posts across restarts.

CREATE TABLE status_message (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    channel_id TEXT,
    ts         TEXT,
    backup_at  TEXT,
    backup_ok  INTEGER,
    post_attempt_at TEXT,
    CHECK ((channel_id IS NULL) = (ts IS NULL)),
    CHECK ((backup_at IS NULL) = (backup_ok IS NULL))
) STRICT;
