# M3-other: always offer "other…", and forgiving thread replies

- Milestone: M3 (Brendan request 2026-11-11, during the dry-run week)
- Branch / worktree: `t/M3-other`, `../YNAB_AutoCategorizer.wt/M3-other`
- Depends on: none
- Touches (exclusive while running): `slack/` (questions.go and its tests); `store/` only if a read query must be added
- Plan refs: none needed; read `slack/questions.go` (`questionMessage`, `HandleClick` actionOther/actionSelect,
  `HandleReply`, `matchCategory`)
- YNAB access: none
- Deploy: yes (steps below)

## Goal
Brendan can't answer a question when the right category isn't one of its few options. "other…" only renders when
`qu.Options` has more than `topN` (3) entries, and its select lists `qu.Options` only. A thread reply works only for
an exact category name, and nothing tells him that. After this task, every open question can be answered with any
visible category, either by a select or by a loosely typed reply in the thread.

## Scope
1. **"other…" on every question.** Always render it. On click, the select lists **every visible category in the plan**,
   grouped by category group, with the question's options first. That's the same set `matchCategory` already falls back to
   (not deleted or hidden, group not deleted or hidden). Block Kit's 100-option cap: use `option_groups` if it helps;
   if there are still more than 100, keep the options plus the first ones by group order and log a WARN with the count.
   A select value must resolve to a category id **server-side** (e.g. `qid:c:<n>`, an index into a list rebuilt
   deterministically at click time, or a store lookup). Never trust a raw id from the payload without checking that it's
   a visible category in the question's plan.
2. **Forgiving thread replies.** `matchCategory` accepts, in order, stopping at the first that gives exactly one match:
   exact name (today); the name with case, punctuation, "&"/"and" and the "Subs - "/"Home - "-style prefix ignored;
   a reply that contains exactly one category name as whole words (e.g. "this was groceries at costco" → Groceries); a
   unique prefix of 4 or more characters. With no match, or with several, reply in the thread with up to 3 closest
   names and a line saying "reply with one of these, or use other…". Don't answer the question in that case.
3. **A hint in every question message:** one context line, "Not listed? Use other…, or reply in the thread with the
   category name."
4. A transaction answered this way follows today's path unchanged: `dry_run_check` is recorded only; `categorize` goes
   through `writer.Apply`.

## Out of scope
- LLM or headless-claude interpretation of free text. That's M5's Slack free text; keep this deterministic.
- Re-rendering questions already posted (new posts and any re-render get the new layout).
- Creating categories from Slack.

## Acceptance (each must be demonstrably checked)
- [ ] Unit: a question with 1 option still renders "other…"; its select holds every visible category (hidden, deleted
      and hidden-group ones excluded); a forged select value naming a hidden or foreign category is rejected → pass
- [ ] Unit: replies "groceries", "Groceries!", "dining out", "this was groceries at costco", "groc" (→ prefix, if unique)
      each resolve to one category; "home" (ambiguous) gets a suggestions reply and leaves the question open → pass
- [ ] Unit: the hint line is present on posted questions; nothing logged carries an id or the reply text
- [ ] `go build ./... && go vet ./... && go test ./...` green

## Deploy steps / smoke check (sup)
`./deploy/deploy.sh` from the repo root (ask Brendan first if Claude Code's auto-mode blocks it). Then: healthy, mode
dry_run, 0 ERROR lines since start, `finbot dry-run-report | tail -1` → `writes: 0`. Brendan checks one new question
shows "other…" with the full list.

<!-- Builder appends ## Result and, if needed, ## Handoff below. -->

## Result
- Status: DONE
- What changed:
  - `slack/catmatch.go` (new): `visible`, `selectGroups` (Suggested group = the question's options, then every answerable
    plan category by group via `option_groups`; capped at 100 total, returns the dropped count), `catKey`/`resolveCatKey`
    (a select option for a plan category carries `<qid>:c:<first 16 hex of sha256(category id)>`, so no YNAB id is in
    the message; it resolves only against the question's plan's visible categories at click time), `norm`, `entries`,
    `matchStages`, `suggest` (Levenshtein, prefix hits first).
  - `slack/questions.go`: "other…" always renders; clicking it loads `CategoriesWithGroups` and swaps in the grouped
    select (WARN `dropped`/`limit` if truncated). `actionSelect` with a `c:` value → `answerCatKey`; pick buttons still take
    only `<qid>:<index>`. `matchCategory` keeps today's exact step, then folded name (case, punctuation, `&`/`and`,
    `X - ` prefix), whole-word containment, unique prefix ≥4. No single match → plain_text thread reply with up to 3
    quoted names and "Reply with one of these, or use other…"; question stays open. Context block `q_hint` with
    "Not listed? Use other…, or reply in the thread with the category name." on every open-question render.
  - Answers go through the unchanged `answer` path (dry_run_check recorded only; categorize via `writer.Apply`).
  - Tests: `slack/questions_other_test.go` (new); `questions_test.go` `blockValues` reads `option_groups`, and the
    non-matching-reply test now uses an unmatchable reply (its old "groceries I think" now resolves, by design).
- Acceptance:
  - 1-option question renders "other…"; select = every visible category (hidden, deleted, hidden-group, foreign
    excluded); forged hidden/deleted/hidden-group/foreign keys, raw ids and junk rejected →
    pass (`go test ./slack -run 'TestOneOptionStillOffersEveryVisibleCategory|TestSelectCapsAtBlockKitLimit'`)
  - "groceries", "Groceries!", "dining out", "this was groceries at costco", "groc" (plus "home and garden",
    "netflix" for `Subs - Netflix`) each resolve to one category; "home" (Home & Garden vs Home - Repairs) gets a
    suggestions reply and stays open → pass (`go test ./slack -run 'TestLooseRepliesResolve|TestAmbiguousReply'`)
  - Hint on posted questions and the select re-render; logs checked for ids, keys and reply text → pass (same tests)
  - (fix) The select and every reply stage use `classify.Eligible`: internal categories (Inflow: Ready to Assign,
    Uncategorized) and the Credit Card Payments group never appear, and their forged keys are rejected. Exact and loose
    replies naming them ("Inflow: Ready to Assign", "Uncategorized", "Visa Card", "visa", "ready to assign") get the
    suggestions reply, never name them, and leave the question open. A set-aside (Gifts) is still listed and
    answerable → pass (`go test ./slack -run 'TestExcludedCategoriesNeverAnswer|TestSelectKeepsSetAsides|TestOneOption'`).
    With the old visible-only filter, those tests fail.
  - (review fix 1, 2) A question's stored options are re-checked against the plan's answerable categories on every
    path: `liveOptions` blanks any option whose category is no longer `classify.Eligible` (hidden, deleted, moved to an
    excluded group) before posting, on every click (pick buttons, index select values, `c:` keys), on the other…
    re-render (buttons and Suggested group) and in `matchCategory` (exact options loop, loose stages, suggestions).
    A rejected click is logged like a forged one (WARN, no id); a reply falls through to suggestions, which no longer
    name the stale option → pass (`go test ./slack -run 'TestOptionInvalidatedAfterAskIsRejected|TestAskSkipsOptionsAlreadyIneligible'`;
    both fail with `liveOptions` made a no-op).
  - `go build ./... && go vet ./... && go test ./...` → pass
- Deviations from brief: the select value is a hash key of the category id rather than an index into a rebuilt list,
  so a category added or renamed between render and click can't shift the answer to a different category. It is
  still resolved server-side against the plan's answerable categories, and the message carries no YNAB id.
  Also touched `classify/candidates.go` (sup's fix request): exported `classify.Eligible` (not deleted or hidden, group
  neither, not internal, not in Credit Card Payments; set-asides allowed), which `Candidates` now uses too, so the
  classifier and Slack share one eligibility rule. No store change was needed.
- Follow-ups for backlog: none
- Docs follow-ups: CLAUDE.md `slack/` bullet: "`Questions` renders a question (top-3 buttons, other… always, split, and
  a hint line); other… opens a select of every answerable plan category (`classify.Eligible`), options first; thread replies match a category
  loosely (exact, folded, whole words, unique 4+ prefix), else suggest up to 3 names."
