# M3 dry-run: evidence for the go-live decision

Collected 2026-11-22 (Denver), read-only: `finbot dry-run-report` and `docker logs`/`docker inspect` on hoid, plus a
read-only copy of hoid's DB (the `hoid_check.sh` copy, `PRAGMA quick_check` ok). Mode on hoid: `CLASSIFY_MODE=dry_run`
throughout. No calibration was run and the mode wasn't changed.

## Recommendation: not go. Hide the old groups, then rerun calibration

Don't turn on `auto` yet. Three things block it, and any one of them is enough:

1. **No usable calibration.** `calibration.json` has `tau: null` and `auto_eligible: false`, and its policy and
   candidates hashes are both from before the 11-12 restructure (see "Hashes" below). Set to `auto` today, finbot
   would refuse and run as `dry_run` with a warning.
2. **Measured precision is below target.** 31 of 39 judged would-apply answers were right: **79.5%** (Wilson 95%:
   **64.5% to 89.2%**). n = 39 is past the 30 floor, but the interval runs from well below to just above 85%. The
   evidence doesn't confirm 85%, and the point estimate falls short of it.
3. **Retired is still visible**, so `Discretionary`, `Talk About` and YNAB's `Needs a job!` placeholder are still
   eligible answers. The model put `Needs a job!` first 4 times in the window (all asked, none would-apply).

Order: (a) hide Retired, Subscriptions, Monthly Variable Expenses and Target Balance Goals in YNAB. Hiding changes the
candidates hash, so it has to come first. (b) Rerun `finbot calibrate` (BA-20, Brendan's). (c) If the new calibration is
`auto_eligible`, run a few more days of dry-run under the new hash and check precision again before the switch.

## Evidence window

The window counts from the first decision under the current hashes (policy `74884ed1`, candidates `451fbae6`), after
the M3-policy deploy at 2026-11-12 08:31 Denver:

- **First decision 2026-11-12, last 2026-11-22: 10 days** (106 decisions, all current, none superseded).
- **100 decisions were superseded by the 11-12 change:** 50 under the original hashes (2026-11-11 evening) and 50 under
  the brief in-between candidates hash from the restructure (2026-11-12 08:30, one minute before the policy deploy).
  One decision from the original hashes is still current, because it was never re-decided. 58 dry-run questions from
  those decisions were closed as `superseded` on 11-12.

## Report output

On hoid, `finbot dry-run-report` covers every decision, superseded ones included (207 since 2026-11-11):

```
decisions: 207 (first 2026-11-11, last 2026-11-22)
by method: rule 26, model 181
by outcome: would_apply 94, asked 87, skipped 26, applied 0
would_apply: 94
judged: 153 (would_apply 79, asked 74)
precision (would_apply): 64/79 = 81.0% (Wilson 95%: 71.0% to 88.1%; target 85.0%)
top-3 hit rate (asked): 41/74 = 55.4%
would_apply misses (predicted → label):
  Voice Lessons → Home - Kids Classes & Education: 4
  Discretionary → Shopping: 3
  Discretionary → (not in the mirror): 2
  Groceries → Household & Kids: 2
  Groceries → Dining Out: 1
  Household & Kids → (not in the mirror): 1
  Shopping → (not in the mirror): 1
  Subs - Amazon Prime → Shopping: 1
questions: 5 open, 23 pending
model errors: 0 (failed second opinions; a failed primary call leaves no decision)
writes: 0
```

The same report code (`calibrate.BuildDryRunReport`), run on the DB copy with only the current-hash decisions. **This
is the evidence window:**

```
decisions: 106 (first 2026-11-12, last 2026-11-22)
by method: rule 14, model 92
by outcome: would_apply 54, asked 38, skipped 14, applied 0
would_apply: 54
judged: 64 (would_apply 39, asked 25)
precision (would_apply): 31/39 = 79.5% (Wilson 95%: 64.5% to 89.2%; target 85.0%)
top-3 hit rate (asked): 16/25 = 64.0%
would_apply misses (predicted → label):
  Groceries → Household & Kids: 2
  Voice Lessons → Home - Kids Classes & Education: 2
  Groceries → Dining Out: 1
  Household & Kids → (not in the mirror): 1
  Shopping → (not in the mirror): 1
  Subs - Amazon Prime → Shopping: 1
questions: 5 open, 23 pending
model errors: 0 (failed second opinions; a failed primary call leaves no decision)
writes: 0
```

Notes:
- **`writes: 0`** on hoid, and `write_log` has 0 rows. No decision is `applied`, and none is in `auto` mode.
- The superseded decisions scored 33/40 (82.5%). The `Discretionary → Shopping` misses all came before the
  restructure. The misses since then are near neighbours (Groceries vs Household & Kids or Dining Out, Voice Lessons
  vs Kids Classes).
- All 14 "rule" decisions in the window are guardrail skips (8 transfer, 3 Ready to Assign, 2 credit card payment,
  1 P2P). No payee rule fired in the window, so every answer came from the model.
- Top-3 hit rate on asked questions was 64.0% (16/25), against 78.2% in the calibration run.

## Model errors and restarts

- **Model errors: 0.** 0 failed second opinions out of 38 (the report's count). **0 `ERROR` lines** in
  `docker logs --since 240h`, and 0 in the container's whole log, which starts 2026-11-12 08:31 Denver and so covers
  the whole window (240h falls about 7h short of it).
- WARN lines in the whole log: 42. 39 are `slack socket` (25 `incoming_error`, 13 `connection_error` reconnects in
  the 240h view), and 3 are `status: slack update failed`. Slack recovered by itself each time.
- **Restarts: 0.** `docker inspect`: `RestartCount=0`, health `healthy`. The container was created and started
  2026-11-12 15:31 UTC (the M3-policy deploy), which is the start of the window.

## Slack noise

The cap of 5 open questions sets the pace: a question is posted only after one is answered.

| Denver day | posted |
|---|---|
| 2026-11-12 (from 08:31) | 39 |
| 2026-11-13 | 5 |
| 2026-11-14 | 10 |
| 2026-11-15 to 11-17 | 0 |
| 2026-11-18 | 5 |
| 2026-11-19 | 10 |
| 2026-11-20 to 11-22 | 0 |

That's 69 posted in the window, about 7 a day. Brendan answered 64 of them; the other 5 have been open since 11-19,
and 23 more wait as `pending`. The 11-12 burst was the backlog under the new hash. Alerts: none posted in the window
(the only `alerts` row is the silent freshness baseline).

## Calibration in use

`calibrate/calibration.json` (created 2026-11-11 17:32 UTC): model `jev-1.14.0`, **τ = null**, **n = 1056**
(742 tuning, 314 holdout; 523 tuning model-band rows), **`auto_eligible: false`**. No τ reached the 0.85 Wilson-lower
target on the old candidates (tune applied 0). Rule precision was 99.7% (343/344).

## Hashes: did the categories change?

Yes. Both hashes differ from `calibration.json`:

| | calibration.json | current decisions |
|---|---|---|
| policy hash | `0071fb89` | `74884ed1` |
| candidates hash | `6a4e4a8a` | `451fbae6` |

So even an `auto_eligible` calibration from before 11-12 wouldn't apply now. Hiding the old groups will change the
candidates hash again.

## Old groups: not hidden yet

From `category_groups.hidden` in the DB copy (last sync 2026-11-22 22:16 UTC):

| group | hidden |
|---|---|
| Retired | no (3 visible categories: Discretionary, Talk About, Needs a job!) |
| Subscriptions | no (no visible categories left) |
| Monthly Variable Expenses | no (no visible categories left) |
| Target Balance Goals | no (no visible categories left) |

While Retired is visible, `Discretionary` is still an eligible answer and appears in the model's top list.

## Linked accounts

21 open accounts on the active plan: **10 linked**, 0 with a link error at the last sync.
