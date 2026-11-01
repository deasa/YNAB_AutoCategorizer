package cmd

import (
	"context"

	"github.com/deasa/YNAB_AutoCategorizer/status"
)

// Version is the git SHA, set at build time with -ldflags -X.
var Version = "dev"

// addStatus makes sched's sync and backup jobs update the Slack status message
// when they finish, unless SLACK_BOT_TOKEN is empty. Call it after addBackup. A
// job cancelled by shutdown reports nothing. The updater only logs its own
// failures, so a job's result is unchanged.
func (a *app) addStatus(sched *Scheduler) {
	cfg := a.cfg
	if cfg.SlackBotToken == "" {
		a.log.Info("slack status disabled")
		return
	}
	updater := status.New(status.Config{
		Token:         cfg.SlackBotToken,
		Channel:       cfg.SlackStatusChannel,
		PlanID:        cfg.YNABBudgetID,
		Store:         a.db,
		Log:           a.log,
		Redact:        a.redact,
		Version:       Version,
		ImportEnabled: cfg.ImportEnabled,
		BackupEnabled: sched.Backup != nil,
		StaleAfter:    2 * cfg.SyncInterval,
		BaseURL:       a.slackURL,
	})
	syncJob := sched.Sync
	sched.Sync = func(ctx context.Context) error {
		err := syncJob(ctx)
		if ctx.Err() == nil {
			updater.SyncDone(ctx, err)
		}
		return err
	}
	if backupJob := sched.Backup; backupJob != nil {
		sched.Backup = func(ctx context.Context) error {
			err := backupJob(ctx)
			if ctx.Err() == nil {
				updater.BackupDone(ctx, err)
			}
			return err
		}
	}
	a.log.Info("slack status enabled", "channel", cfg.SlackStatusChannel)
}
