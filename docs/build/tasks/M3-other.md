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
