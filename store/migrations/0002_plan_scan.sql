-- 0002: result of the one-shot archived-plan scan (architecture.md §3.1,
-- R8c): which plans the token reaches carry a "Discretionary" category and
-- so hold labelled history. One row per plan from the latest scan; a scan
-- replaces the whole table. A plan whose categories couldn't be fetched has
-- has_discretionary NULL and scan_error set (redacted: no token or ids).

CREATE TABLE plan_scan (
    plan_id           TEXT    NOT NULL PRIMARY KEY,
    name              TEXT    NOT NULL,
    has_discretionary INTEGER,
    scan_error        TEXT,
    scanned_at        TEXT    NOT NULL,
    CHECK ((has_discretionary IS NULL) != (scan_error IS NULL))
) STRICT;
