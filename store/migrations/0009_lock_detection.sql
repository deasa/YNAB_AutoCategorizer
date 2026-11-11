-- 0009: which plans have had their one-time human-lock backfill (M3-9; §3.1
-- step 2, R6b). A delta carries only changed rows, so a plan whose mirror
-- predates lock detection gets one pass over every transaction; this row
-- records that the pass committed, in the same transaction as its locks.

CREATE TABLE lock_detection (
    plan_id       TEXT PRIMARY KEY,
    backfilled_at TEXT NOT NULL
) STRICT;
