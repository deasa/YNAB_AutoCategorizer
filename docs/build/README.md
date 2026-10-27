# Build system — start here

finbot (the evolution of this repo) is built entirely by AI agents in herdr panes. This folder is how they coordinate.
What to build: `docs/plan/requirements.md` (v5) and `docs/plan/architecture.md` (a4). Both are final; don't re-litigate them.
Changing them requires Brendan.

| File | Who reads it | Purpose |
|---|---|---|
| `SUPERVISOR.md` | supervisor | Role, loop, dispatch/merge procedure, context and rate-limit rules, handoff |
| `STATE.md` | supervisor (always first), Brendan | **Live source of truth**: milestone, tasks, panes, blockers, Brendan actions, decisions |
| `BACKLOG.md` | supervisor | Work packages per milestone, dependencies, what each touches |
| `WORKER.md` | every builder pane | How a builder works, checks its context, finishes, and hands off |
| `REVIEWER.md` | agy panes | Adversarial review and independent testing protocol |
| `tasks/<ID>.md` | the one builder on that task | The brief, with result and handoff notes appended by the builder |
| `handoffs/` | the next supervisor | Supervisor handoff notes, one file per supervisor session |
| `../../scripts/build/panes.sh` | supervisor | `status`, `ctx <pane>`, `watch` (background wait for events) |

## Roles

- **Supervisor**: one Claude Opus 5.5 pane named `sup`. It watches, dispatches, unblocks and merges. It doesn't write product code.
- **Builders**: Claude Opus 5.5 panes named `b-<task-id>`. Each works on one task in its own git worktree and branch.
- **agy**: one Antigravity pane named `agy`. It does adversarial review of every branch before merge, plus independent black-box
  testing at milestone ends.
- **Brendan**: does the human-only actions listed in `STATE.md` → *Brendan actions*, and approves anything the docs mark as
  needing Brendan.

## Hard rules (all agents)

1. **No agent works above 40% of its context window.** Builders stop starting new work at 30%. At 35% they write a handoff
   note and stop by 40% at the latest. See `WORKER.md`.
2. All work happens in herdr panes in workspace `w1`: tab `control` (sup, agy) and tab `build` (builders).
3. Never move real money. Never write to Brendan's live YNAB plan unless a task brief explicitly allows it. The default is
   read-only, and write tests use the sandbox plan.
4. Secrets are never printed, pasted into chat or committed. `.env` files are gitignored and copied into worktrees by the
   supervisor.
5. Only the supervisor merges to `main`, pushes, and deploys to hoid.

## Bootstrap prompt for a new supervisor (Brendan pastes this into a fresh Claude Opus 5.5 pane)

```
You are the build supervisor for finbot. Your working directory is ~/dev/github.com/deasa/YNAB_AutoCategorizer
(main checkout). Read, in order: docs/build/README.md, docs/build/SUPERVISOR.md, docs/build/STATE.md, and the newest file
in docs/build/handoffs/. Then run scripts/build/panes.sh status, reconcile STATE.md against what is actually running, rename
yourself with `herdr agent rename "$HERDR_PANE_ID" sup`, and resume the supervisor loop. Don't read docs/plan/* in full
unless a decision needs it.
```
