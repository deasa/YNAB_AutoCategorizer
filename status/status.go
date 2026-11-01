// Package status keeps one Slack message current: finbot edits it after every
// sync tick and backup run, so its content and its "edited" time always show
// the latest state. It is a heartbeat, not an alert channel. Slack failures are
// only logged and never fail the job that triggered them.
package status

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	defaultBaseURL = "https://slack.com/api"
	// runTimeout bounds one update, so Slack can't stall the scheduler for long.
	runTimeout = 20 * time.Second
	// maxErrorLen caps the last-error line, in runes.
	maxErrorLen = 300
)

// Store is the part of store.DB the updater uses.
type Store interface {
	GetStatusState(ctx context.Context) (store.StatusState, error)
	SetStatusMessage(ctx context.Context, channelID, ts string) error
	SetStatusBackup(ctx context.Context, at time.Time, ok bool) error
	GetSyncState(ctx context.Context, planID string) (store.SyncState, bool, error)
	AccountImports(ctx context.Context, planID string) ([]store.AccountImport, error)
}

// Config sets up an Updater.
type Config struct {
	Token   string // the bot token
	Channel string // private channel name
	PlanID  string // the active plan
	Store   Store
	Log     *slog.Logger
	// Redact scrubs ids and secrets from an error before it is logged or shown.
	Redact func(string) string

	Version       string
	ImportEnabled bool
	BackupEnabled bool
	// StaleAfter is how old the last successful sync may get before the state
	// is failing.
	StaleAfter time.Duration

	BaseURL string           // defaults to the Slack API (tests)
	Client  *http.Client     // defaults to a plain client (tests)
	Now     func() time.Time // defaults to time.Now (tests)
}

// Updater edits the status message. It is not safe for concurrent use; the
// scheduler calls it from its one goroutine. A nil Updater does nothing.
type Updater struct {
	cfg   Config
	slack *slack
	loc   *time.Location

	loaded        bool
	channel       string // resolved from Config.Channel, cached
	stored        store.StatusState
	lastSyncOK    time.Time
	lastSyncErr   string
	lastBackupErr string
}

// New returns an Updater, or nil if cfg.Token is empty (disabled).
func New(cfg Config) *Updater {
	if cfg.Token == "" {
		return nil
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	loc, err := time.LoadLocation("America/Denver")
	if err != nil {
		loc = time.Local
	}
	return &Updater{
		cfg:   cfg,
		slack: &slack{token: cfg.Token, baseURL: cfg.BaseURL, client: cfg.Client},
		loc:   loc,
	}
}

// SyncDone records one sync tick's outcome and updates the message. err is nil
// after a success; a skipped tick is an err too (not a fresh sync).
func (u *Updater) SyncDone(ctx context.Context, err error) {
	if u == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	u.load(ctx)
	if err != nil {
		u.lastSyncErr = u.oneLine(err.Error())
	} else {
		u.lastSyncErr = ""
		u.lastSyncOK = u.cfg.Now()
	}
	u.update(ctx)
}

// BackupDone records one backup run's outcome and updates the message.
func (u *Updater) BackupDone(ctx context.Context, err error) {
	if u == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	u.load(ctx)
	u.lastBackupErr = ""
	if err != nil {
		u.lastBackupErr = u.oneLine(err.Error())
	}
	u.stored.BackupAt, u.stored.BackupOK = u.cfg.Now(), err == nil
	if e := u.cfg.Store.SetStatusBackup(ctx, u.stored.BackupAt, u.stored.BackupOK); e != nil {
		u.warn("status: store backup result failed", e)
	}
	u.update(ctx)
}

// load reads the stored state once per process. Until it succeeds, the updater
// retries on the next run rather than post a second message.
func (u *Updater) load(ctx context.Context) {
	if u.loaded {
		return
	}
	stored, err := u.cfg.Store.GetStatusState(ctx)
	if err != nil {
		u.warn("status: read stored state failed", err)
		return
	}
	u.stored = stored
	if s, ok, err := u.cfg.Store.GetSyncState(ctx, u.cfg.PlanID); err != nil {
		u.warn("status: read sync state failed", err)
		return
	} else if ok {
		u.lastSyncOK = s.SyncedAt
	}
	u.loaded = true
}

// update posts or edits the message. Failures are only logged.
func (u *Updater) update(ctx context.Context) {
	if !u.loaded {
		return
	}
	if err := u.send(ctx, u.render(ctx)); err != nil {
		u.warn("status: slack update failed", err)
	}
}

func (u *Updater) send(ctx context.Context, text string) error {
	if u.channel == "" {
		id, err := u.slack.findPrivateChannel(ctx, u.cfg.Channel)
		if err != nil {
			return err
		}
		u.channel = id
	}
	// A stored message in another channel means SLACK_STATUS_CHANNEL changed.
	if u.stored.TS != "" && u.stored.ChannelID == u.channel {
		_, err := u.slack.send(ctx, u.channel, u.stored.TS, text)
		if err == nil {
			return nil
		}
		if !isCode(err, "message_not_found", "channel_not_found") {
			return err
		}
		u.cfg.Log.Warn("status: message gone, posting a new one", "reason", u.redact(err.Error()))
		if isCode(err, "channel_not_found") {
			u.channel = ""
			id, err := u.slack.findPrivateChannel(ctx, u.cfg.Channel)
			if err != nil {
				return err
			}
			u.channel = id
		}
	}
	ts, err := u.slack.send(ctx, u.channel, "", text)
	if err != nil {
		return err
	}
	u.stored.ChannelID, u.stored.TS = u.channel, ts
	u.cfg.Log.Info("status message created; pin it in Slack", "channel", u.cfg.Channel)
	// The in-memory copy still stops a repost in this process if this fails.
	if err := u.cfg.Store.SetStatusMessage(ctx, u.channel, ts); err != nil {
		u.warn("status: store message id failed", err)
	}
	return nil
}

// render builds the message. Everything from outside finbot goes through
// escape, so an account name can't ping a channel or form a link. The updated
// line changes on every run: Slack needs a real edit to advance "edited".
func (u *Updater) render(ctx context.Context) string {
	accounts, err := u.cfg.Store.AccountImports(ctx, u.cfg.PlanID)
	if err != nil {
		u.warn("status: read accounts failed", err)
	}
	now := u.cfg.Now()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", u.state(now, accounts))
	fmt.Fprintf(&b, "*Updated:* %s\n", now.In(u.loc).Format("Mon Jan 2, 3:04:05 PM MST"))
	fmt.Fprintf(&b, "*Last successful sync:* %s\n", u.when(u.lastSyncOK))
	fmt.Fprintf(&b, "*Last error:* %s\n", u.lastError())
	fmt.Fprintf(&b, "*Last backup:* %s\n", u.backup())
	fmt.Fprintf(&b, "*Import:* %s\n", onOff(u.cfg.ImportEnabled))
	fmt.Fprintf(&b, "*Version:* %s\n", escape(u.cfg.Version))
	switch {
	case err != nil:
		b.WriteString("*Accounts:* unavailable")
	case len(accounts) == 0:
		b.WriteString("*Accounts:* none linked")
	default:
		b.WriteString("*Accounts (newest import):*")
		for _, a := range accounts {
			date := a.NewestImport
			if date == "" {
				date = "no imports"
			}
			fmt.Fprintf(&b, "\n• %s: %s", escape(a.Name), date)
			if a.InError {
				b.WriteString(" :warning: link error")
			}
		}
	}
	return b.String()
}

// state is failing once the last success is older than StaleAfter (what makes
// /healthz answer 503), and degraded while anything else needs attention.
func (u *Updater) state(now time.Time, accounts []store.AccountImport) string {
	if u.lastSyncOK.IsZero() || now.Sub(u.lastSyncOK) > u.cfg.StaleAfter {
		return ":red_circle: *finbot: failing*"
	}
	degraded := u.lastSyncErr != "" || (!u.stored.BackupAt.IsZero() && !u.stored.BackupOK)
	for _, a := range accounts {
		degraded = degraded || a.InError
	}
	if degraded {
		return ":large_yellow_circle: *finbot: degraded*"
	}
	return ":large_green_circle: *finbot: OK*"
}

func (u *Updater) when(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.In(u.loc).Format("Mon Jan 2, 3:04 PM MST")
}

func (u *Updater) lastError() string {
	switch {
	case u.lastSyncErr != "":
		return escape(u.lastSyncErr)
	case u.lastBackupErr != "":
		return "backup: " + escape(u.lastBackupErr)
	}
	return "none"
}

func (u *Updater) backup() string {
	switch {
	case !u.cfg.BackupEnabled:
		return "disabled"
	case u.stored.BackupAt.IsZero():
		return "none yet"
	case u.stored.BackupOK:
		return u.when(u.stored.BackupAt) + ", ok"
	}
	return u.when(u.stored.BackupAt) + ", failed"
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func (u *Updater) warn(msg string, err error) {
	u.cfg.Log.Warn(msg, "err", u.redact(err.Error()))
}

var (
	tokenRE  = regexp.MustCompile(`xox[a-zA-Z]-[A-Za-z0-9-]+`)
	amountRE = regexp.MustCompile(`[$€£]\s?[\d,]+(\.\d+)?|\b\d[\d,]*\.\d{2}\b`)
)

// redact removes the bot token, and anything Config.Redact knows about.
func (u *Updater) redact(s string) string {
	s = strings.ReplaceAll(s, u.cfg.Token, "<secret>")
	s = tokenRE.ReplaceAllString(s, "<secret>")
	if u.cfg.Redact != nil {
		s = u.cfg.Redact(s)
	}
	return s
}

// oneLine makes an error fit the message: redacted, no amounts, one short line.
func (u *Updater) oneLine(s string) string {
	s = amountRE.ReplaceAllString(u.redact(s), "<amount>")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxErrorLen {
		s = string(r[:maxErrorLen]) + "…"
	}
	return s
}

// escape neutralizes Slack's control characters.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
