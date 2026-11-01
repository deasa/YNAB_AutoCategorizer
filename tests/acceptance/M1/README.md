# M1 acceptance scripts

Run from the repo root. All of them are read-only against YNAB, Slack and hoid. None prints a secret, but count
matches with `secret_count` from `lib.sh` before showing any output.

- `hoid_state.sh`: container hardening, health, env names, processes (item 1).
- `watch.sh OUTDIR MINUTES`: polls hoid logs, health and the Slack status message every 5 min (items 2, 4).
- `slack_status.py [--text]`: conversations.list and conversations.history only (item 4).
- `import_gate.sh BIN WORKDIR`: import off and on, with bogus credentials and a dead proxy, so it cannot write (item 2).
- `restore_check.sh BIN KEY WORKDIR`: restores the newest backup, checks integrity and counts, tries a wrong key, then
  deletes the files (item 5).
