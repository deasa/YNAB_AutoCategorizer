package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/deasa/YNAB_AutoCategorizer/alerts"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/slack"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
)

// socketClient is what serve needs from the Slack Socket Mode client.
type socketClient interface {
	// SelfCheck resolves the configured channel.
	SelfCheck(ctx context.Context) error
	// Run connects and dispatches events until ctx ends (nil) or the socket
	// fails.
	Run(ctx context.Context) error
	// PostAlert posts text to the channel. Its error is safe to log.
	PostAlert(ctx context.Context, text string) error
}

// newSocketClient builds the Socket Mode client (tests replace it, so a serve
// test never reaches Slack).
var newSocketClient = func(cfg *config.Config, st slack.Store, log *slog.Logger, redact func(string) string) (socketClient, error) {
	c, err := slack.NewFromTokens(cfg.SlackAppToken, cfg.SlackBotToken, slack.Config{Channel: cfg.SlackStatusChannel},
		st, slack.Handlers{}, log)
	if err != nil {
		return nil, err
	}
	return &slackSocket{Client: c, api: slackapi.New(cfg.SlackBotToken), channel: cfg.SlackStatusChannel, redact: redact}, nil
}

// slackSocket is the real socketClient.
type slackSocket struct {
	*slack.Client
	api     *slackapi.Client
	channel string
	redact  func(string) string
}

// PostAlert posts through the client once its channel is resolved. Before
// that (the self-check failed) it posts by channel name, so an alert can still
// get through when only the lookup is broken, such as a missing scope.
func (s *slackSocket) PostAlert(ctx context.Context, text string) error {
	if s.ChannelID() != "" {
		_, err := s.PostBlocks(ctx, "", text, nil)
		return err
	}
	_, _, err := s.api.PostMessageContext(ctx, s.channel,
		slackapi.MsgOptionText(text, true), slackapi.MsgOptionDisableMarkdown())
	if err != nil {
		return fmt.Errorf("slack: post by channel name: %s", s.redact(err.Error()))
	}
	return nil
}

// alertPoster fits a socketClient to alerts.Poster.
type alertPoster struct{ c socketClient }

func (p alertPoster) Post(ctx context.Context, text string) error { return p.c.PostAlert(ctx, text) }

// alerter is what serve feeds; *alerts.Alerter meets it.
type alerter interface {
	Freshness(ctx context.Context, initial bool, accounts []finsync.AccountFreshness) error
	Anomalies(ctx context.Context) error
	Send(ctx context.Context, al alerts.Alert) (alerts.Outcome, error)
	Clear(ctx context.Context, kind, dedupeKey string) error
}

// lockedAlerter serializes an alerter: *alerts.Alerter isn't safe for
// concurrent use, and the socket goroutine sends alongside the scheduler.
type lockedAlerter struct {
	mu sync.Mutex
	a  alerter
}

func (l *lockedAlerter) Freshness(ctx context.Context, initial bool, accounts []finsync.AccountFreshness) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.a.Freshness(ctx, initial, accounts)
}

func (l *lockedAlerter) Anomalies(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.a.Anomalies(ctx)
}

func (l *lockedAlerter) Send(ctx context.Context, al alerts.Alert) (alerts.Outcome, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.a.Send(ctx, al)
}

func (l *lockedAlerter) Clear(ctx context.Context, kind, dedupeKey string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.a.Clear(ctx, kind, dedupeKey)
}

// selfCheckKey is the dedupe key of the self-check alert.
const selfCheckKey = "channel"

const selfCheckText = "finbot: the Slack self-check failed: the configured channel didn't resolve, so questions and " +
	"replies are off. Is the bot still a member of the channel? finbot keeps syncing and retries."

var (
	// selfCheckTimeout caps one self-check.
	selfCheckTimeout = 20 * time.Second
	// socketRetryMin and socketRetryMax bound the wait before retrying a
	// failed self-check or socket; it doubles per consecutive failure.
	socketRetryMin = time.Minute
	socketRetryMax = 30 * time.Minute
	// socketHealthyRun is how long a socket run must last to reset the wait.
	socketHealthyRun = 10 * time.Minute
)

// startSlack sets up Socket Mode and the alerter when SLACK_APP_TOKEN,
// SLACK_BOT_TOKEN and SLACK_USERS are all set: it upserts the users, runs the
// self-check once (a failure raises a critical alert; serve carries on), then
// runs the client under ctx on its own goroutine. The returned channel closes
// when that goroutine ends; it is closed already when Socket Mode is off.
func (a *app) startSlack(ctx context.Context) (<-chan struct{}, error) {
	done := make(chan struct{})
	cfg := a.cfg
	if !cfg.SocketMode() {
		a.log.Info("slack socket mode disabled")
		close(done)
		return done, nil
	}
	users := make([]store.User, 0, len(cfg.SlackUsers))
	for _, u := range cfg.SlackUsers {
		users = append(users, store.User{SlackUserID: u.SlackID, Label: u.Label})
	}
	if err := a.db.UpsertUsers(ctx, users, time.Now()); err != nil {
		return nil, err
	}
	sock, err := newSocketClient(cfg, a.db, a.log, a.redact)
	if err != nil {
		// The mirror matters more than Slack: log it and carry on without.
		a.log.Error("slack socket mode disabled: bad config", "err", a.redact(err.Error()))
		close(done)
		return done, nil
	}
	a.socket = sock
	a.alerter = &lockedAlerter{a: alerts.New(alerts.Config{
		Store: a.db, Poster: alertPoster{sock}, PlanID: cfg.YNABBudgetID, Mirror: a.db, Logger: a.log,
	})}
	a.log.Info("slack socket mode enabled", "channel", cfg.SlackStatusChannel, "users", len(users))

	ok := a.selfCheck(ctx)
	go func() {
		defer close(done)
		a.runSocket(ctx, ok)
	}()
	return done, nil
}

// selfCheck runs the self-check once. A failure is logged and raises the
// critical alert; a pass clears it, so a later failure alerts again.
func (a *app) selfCheck(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, selfCheckTimeout)
	err := a.socket.SelfCheck(cctx)
	cancel()
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		a.log.Error("slack self-check failed", "err", a.redact(err.Error()))
		out, aerr := a.alerter.Send(ctx, alerts.Alert{Kind: alerts.KindSocketSelfCheck, DedupeKey: selfCheckKey, Text: selfCheckText})
		if aerr != nil {
			a.log.Error("self-check alert failed", "err", a.redact(aerr.Error()))
		} else {
			a.log.Info("self-check alert", "outcome", out.String())
		}
		return false
	}
	if err := a.alerter.Clear(ctx, alerts.KindSocketSelfCheck, selfCheckKey); err != nil {
		a.log.Error("clear self-check alert", "err", a.redact(err.Error()))
	}
	a.log.Info("slack self-check ok")
	return true
}

// runSocket keeps the client running until ctx ends: it retries a failed
// self-check, and restarts a socket that failed, after a growing wait.
func (a *app) runSocket(ctx context.Context, checked bool) {
	wait := socketRetryMin
	backoff := func() bool {
		t := time.NewTimer(wait)
		defer t.Stop()
		wait = min(2*wait, socketRetryMax)
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	for ctx.Err() == nil {
		if !checked {
			if checked = a.selfCheck(ctx); !checked {
				if !backoff() {
					return
				}
				continue
			}
		}
		started := time.Now()
		err := a.socket.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("socket stopped")
		}
		a.log.Error("slack socket mode stopped; will retry", "err", a.redact(err.Error()))
		if time.Since(started) >= socketHealthyRun {
			wait = socketRetryMin
		}
		if !backoff() {
			return
		}
	}
}

// feedAlerts runs the freshness and anomaly alerts after a successful sync of
// the active plan. Outcomes are only logged: they never fail the sync job or
// touch Health.
func (a *app) feedAlerts(ctx context.Context, res finsync.Result) {
	if a.alerter == nil {
		return
	}
	accounts, err := a.syncer.Freshness(ctx, a.cfg.YNABBudgetID)
	if err == nil {
		err = a.alerter.Freshness(ctx, res.Initial, accounts)
	}
	if err != nil {
		a.log.Error("freshness alerts failed", "err", a.redact(err.Error()))
	}
	if err := a.alerter.Anomalies(ctx); err != nil {
		a.log.Error("anomaly alerts failed", "err", a.redact(err.Error()))
	}
}
