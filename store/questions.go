package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Values of Question.Kind and Status, and of User.Label. The tables' CHECKs
// enforce them. A user label is also a write_log actor.
const (
	QuestionCategorize  = "categorize"
	QuestionDryRunCheck = "dry_run_check"

	QuestionPending  = "pending" // decided, not yet posted because of the cap
	QuestionOpen     = "open"
	QuestionAnswered = "answered"
	QuestionClosed   = "closed"

	UserBrendan = ActorBrendan
	UserWife    = ActorWife
)

// ErrQuestionNotPending means a question can't be marked posted: it isn't
// pending, or no such row exists.
var ErrQuestionNotPending = errors.New("store: question is not pending")

// QuestionOption is one choice offered with a question. CategoryID and
// CategoryName are empty for a choice that isn't a category ("other…").
type QuestionOption struct {
	Label        string `json:"label"`
	CategoryID   string `json:"category_id,omitempty"`
	CategoryName string `json:"category_name,omitempty"`
}

// Question is one question asked (or to be asked) in Slack.
type Question struct {
	ID          int64 // set by InsertQuestion
	PlanID      string
	Kind        string
	SubjectID   string  // e.g. the transaction id
	ChannelID   *string // nil until posted
	MessageTS   *string // nil until posted
	Options     []QuestionOption
	Status      string
	Answer      *string
	AnsweredBy  *string // a user label
	AnsweredAt  *time.Time
	CloseReason *string
	CreatedAt   time.Time // InsertQuestion uses now if zero
}

// InsertQuestion stores q as pending or, when q.ChannelID and q.MessageTS are
// set, as open, and returns its id. Status, answer and close fields are
// ignored. A second live question for the same plan, kind and subject fails.
func (d *DB) InsertQuestion(ctx context.Context, q Question) (int64, error) {
	created := q.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	status := QuestionPending
	if q.ChannelID != nil || q.MessageTS != nil {
		status = QuestionOpen
	}
	opts := q.Options
	if opts == nil {
		opts = []QuestionOption{}
	}
	body, err := json.Marshal(opts)
	if err != nil {
		return 0, fmt.Errorf("store: marshal question options: %w", err)
	}
	var id int64
	err = writeTx(ctx, d.w, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx, `
INSERT INTO questions (plan_id, kind, subject_id, channel_id, message_ts, options, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			q.PlanID, q.Kind, q.SubjectID, q.ChannelID, q.MessageTS, string(body), status, formatTime(created))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: insert question: %w", err)
	}
	return id, nil
}

// MarkQuestionPosted moves a pending question to open at the Slack message
// that carries it. It returns ErrQuestionNotPending if id is not pending.
func (d *DB) MarkQuestionPosted(ctx context.Context, id int64, channelID, messageTS string) error {
	n, err := d.execWrite(ctx, `
UPDATE questions SET status = 'open', channel_id = ?, message_ts = ?
WHERE id = ? AND status = 'pending'`, channelID, messageTS, id)
	if err != nil {
		return fmt.Errorf("store: mark question posted: %w", err)
	}
	if n == 0 {
		return ErrQuestionNotPending
	}
	return nil
}

// ClaimAnswer records answer as the question's answer if it is still open, in
// one statement, and reports whether this caller won. A redelivered click, a
// second person or a question already closed all get false.
func (d *DB) ClaimAnswer(ctx context.Context, id int64, answer, answeredBy string, at time.Time) (bool, error) {
	n, err := d.execWrite(ctx, `
UPDATE questions SET status = 'answered', answer = ?, answered_by = ?, answered_at = ?
WHERE id = ? AND status = 'open'`, answer, answeredBy, formatTime(at), id)
	if err != nil {
		return false, fmt.Errorf("store: claim answer: %w", err)
	}
	return n == 1, nil
}

// CloseQuestion closes a pending or open question with reason (e.g. "superseded",
// "categorized in YNAB") and reports whether it did. An answered or already
// closed question is left alone.
func (d *DB) CloseQuestion(ctx context.Context, id int64, reason string) (bool, error) {
	n, err := d.execWrite(ctx, `
UPDATE questions SET status = 'closed', close_reason = ?
WHERE id = ? AND status IN ('pending', 'open')`, reason, id)
	if err != nil {
		return false, fmt.Errorf("store: close question: %w", err)
	}
	return n == 1, nil
}

// execWrite runs one statement in its own write transaction and returns the
// rows it changed.
func (d *DB) execWrite(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := writeTx(ctx, d.w, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

const questionColumns = `id, plan_id, kind, subject_id, channel_id, message_ts, options, status, answer, answered_by,
       answered_at, close_reason, created_at`

func scanQuestion(scan func(...any) error) (Question, error) {
	var (
		q             Question
		opts, created string
		answeredAt    *string
	)
	if err := scan(&q.ID, &q.PlanID, &q.Kind, &q.SubjectID, &q.ChannelID, &q.MessageTS, &opts, &q.Status, &q.Answer,
		&q.AnsweredBy, &answeredAt, &q.CloseReason, &created); err != nil {
		return Question{}, err
	}
	if err := json.Unmarshal([]byte(opts), &q.Options); err != nil {
		return Question{}, fmt.Errorf("parse options: %w", err)
	}
	at, err := time.Parse(timeLayout, created)
	if err != nil {
		return Question{}, fmt.Errorf("parse created_at: %w", err)
	}
	q.CreatedAt = at
	if answeredAt != nil {
		at, err := time.Parse(timeLayout, *answeredAt)
		if err != nil {
			return Question{}, fmt.Errorf("parse answered_at: %w", err)
		}
		q.AnsweredAt = &at
	}
	return q, nil
}

// QuestionByID returns one question. ok is false if there is none.
func (d *DB) QuestionByID(ctx context.Context, id int64) (q Question, ok bool, err error) {
	return d.oneQuestion(ctx, `WHERE id = ?`, id)
}

// QuestionByMessage returns the question posted as (channelID, messageTS), the
// key a click or thread reply carries. ok is false if there is none.
func (d *DB) QuestionByMessage(ctx context.Context, channelID, messageTS string) (q Question, ok bool, err error) {
	return d.oneQuestion(ctx, `WHERE channel_id = ? AND message_ts = ?`, channelID, messageTS)
}

func (d *DB) oneQuestion(ctx context.Context, where string, args ...any) (Question, bool, error) {
	q, err := scanQuestion(d.r.QueryRowContext(ctx, `SELECT `+questionColumns+` FROM questions `+where, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Question{}, false, nil
	}
	if err != nil {
		return Question{}, false, fmt.Errorf("store: question: %w", err)
	}
	return q, true, nil
}

// OpenQuestions returns planID's open questions, oldest first.
func (d *DB) OpenQuestions(ctx context.Context, planID string) ([]Question, error) {
	return d.questions(ctx, `WHERE plan_id = ? AND status = 'open' ORDER BY id`, planID)
}

// PendingQuestions returns planID's pending questions, oldest first: the order
// to post them in as the cap allows.
func (d *DB) PendingQuestions(ctx context.Context, planID string) ([]Question, error) {
	return d.questions(ctx, `WHERE plan_id = ? AND status = 'pending' ORDER BY id`, planID)
}

// QuestionsBySubject returns every question about subjectID, of any kind and
// status, oldest first.
func (d *DB) QuestionsBySubject(ctx context.Context, planID, subjectID string) ([]Question, error) {
	return d.questions(ctx, `WHERE plan_id = ? AND subject_id = ? ORDER BY id`, planID, subjectID)
}

// AnsweredQuestions returns planID's questions of kind answered in [from, to),
// by answer time.
func (d *DB) AnsweredQuestions(ctx context.Context, planID, kind string, from, to time.Time) ([]Question, error) {
	return d.questions(ctx, `
WHERE plan_id = ? AND kind = ? AND status = 'answered' AND answered_at >= ? AND answered_at < ?
ORDER BY answered_at, id`, planID, kind, formatTime(from), formatTime(to))
}

// CountQuestions returns how many of planID's questions are open and pending.
func (d *DB) CountQuestions(ctx context.Context, planID string) (open, pending int, err error) {
	err = d.r.QueryRowContext(ctx, `
SELECT COALESCE(SUM(status = 'open'), 0), COALESCE(SUM(status = 'pending'), 0)
FROM questions WHERE plan_id = ? AND status IN ('open', 'pending')`, planID).Scan(&open, &pending)
	if err != nil {
		return 0, 0, fmt.Errorf("store: count questions: %w", err)
	}
	return open, pending, nil
}

func (d *DB) questions(ctx context.Context, tail string, args ...any) ([]Question, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT `+questionColumns+` FROM questions `+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("store: questions: %w", err)
	}
	defer rows.Close()
	var out []Question
	for rows.Next() {
		q, err := scanQuestion(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan question: %w", err)
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: questions: %w", err)
	}
	return out, nil
}

// User maps a Slack user id to who that is.
type User struct {
	SlackUserID string
	Label       string // UserBrendan or UserWife
}

// UpsertUsers stores users from config in one transaction. A label given to a
// new Slack id moves to it; a Slack id given a new label takes it.
func (d *DB) UpsertUsers(ctx context.Context, users []User, now time.Time) error {
	err := writeTx(ctx, d.w, func(c *sql.Conn) error {
		for _, u := range users {
			if _, err := c.ExecContext(ctx, `DELETE FROM users WHERE label = ? AND slack_user_id <> ?`,
				u.Label, u.SlackUserID); err != nil {
				return err
			}
			if _, err := c.ExecContext(ctx, `
INSERT INTO users (slack_user_id, label, updated_at) VALUES (?, ?, ?)
ON CONFLICT (slack_user_id) DO UPDATE SET label = excluded.label, updated_at = excluded.updated_at`,
				u.SlackUserID, u.Label, formatTime(now)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: upsert users: %w", err)
	}
	return nil
}

// UserBySlackID returns the user with that Slack id. ok is false for anyone
// else, whose clicks and replies are not acted on.
func (d *DB) UserBySlackID(ctx context.Context, slackUserID string) (u User, ok bool, err error) {
	err = d.r.QueryRowContext(ctx, `SELECT slack_user_id, label FROM users WHERE slack_user_id = ?`, slackUserID).
		Scan(&u.SlackUserID, &u.Label)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("store: user: %w", err)
	}
	return u, true, nil
}

// slackEventTTL is how long a Slack event key is remembered.
const slackEventTTL = 7 * 24 * time.Hour

// SeenSlackEvent records key as handled at now and reports whether it had
// already been seen, in which case the event is a redelivery to drop.
func (d *DB) SeenSlackEvent(ctx context.Context, key string, now time.Time) (duplicate bool, err error) {
	n, err := d.execWrite(ctx, `INSERT INTO slack_events (dedupe_key, seen_at) VALUES (?, ?) ON CONFLICT DO NOTHING`,
		key, formatTime(now))
	if err != nil {
		return false, fmt.Errorf("store: seen slack event: %w", err)
	}
	return n == 0, nil
}

// PruneSlackEvents deletes event keys seen more than 7 days before now and
// returns how many it deleted.
func (d *DB) PruneSlackEvents(ctx context.Context, now time.Time) (int64, error) {
	n, err := d.execWrite(ctx, `DELETE FROM slack_events WHERE seen_at < ?`, formatTime(now.Add(-slackEventTTL)))
	if err != nil {
		return 0, fmt.Errorf("store: prune slack events: %w", err)
	}
	return n, nil
}
