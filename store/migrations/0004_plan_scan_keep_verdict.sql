-- 0004: a plan that fails a scan keeps its last good verdict, so a row can now
-- hold both has_discretionary and scan_error. has_discretionary is NULL only
-- for a failed plan that has never scanned successfully; a success always has
-- a verdict. SQLite can't alter a CHECK, so the table is rebuilt.

CREATE TABLE plan_scan_new (
    plan_id           TEXT    NOT NULL PRIMARY KEY,
    name              TEXT    NOT NULL,
    has_discretionary INTEGER,
    scan_error        TEXT,
    scanned_at        TEXT    NOT NULL,
    CHECK (has_discretionary IS NOT NULL OR scan_error IS NOT NULL)
) STRICT;

INSERT INTO plan_scan_new (plan_id, name, has_discretionary, scan_error, scanned_at)
    SELECT plan_id, name, has_discretionary, scan_error, scanned_at FROM plan_scan;

DROP TABLE plan_scan;

ALTER TABLE plan_scan_new RENAME TO plan_scan;
