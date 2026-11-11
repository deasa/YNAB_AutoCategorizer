package slack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
)

// Reasons a question is closed (questions.close_reason).
const (
	CloseSplit   = "split_in_ynab" // someone pressed "split": they split it in YNAB
	CloseDeleted = "deleted"       // the transaction is deleted in YNAB, or gone from the mirror
	CloseLocked  = "locked"        // a person set the category in YNAB (a category lock)
)

// Action ids on a question message. A pick button's id is actionPick plus its
// slot (0..2), since action ids must be unique within a block.
const (
	actionPick   = "q_pick_"
	actionOther  = "q_other"
	actionSplit  = "q_split"
	actionSelect = "q_select"

	topN       = 3   // pick buttons shown
	maxSelect  = 100 // Block Kit's static_select option limit
	maxBtnText = 75  // Block Kit's button and option text limit
)

// QuestionStore is what Questions needs from the database; *store.DB meets it.
type QuestionStore interface {
	QuestionByID(ctx context.Context, id int64) (store.Question, bool, error)
	QuestionByMessage(ctx context.Context, channelID, messageTS string) (store.Question, bool, error)
	MarkQuestionPosted(ctx context.Context, id int64, channelID, messageTS string) error
	ClaimAnswer(ctx context.Context, id int64, answer, answeredBy string, at time.Time) (bool, error)
	CloseQuestion(ctx context.Context, id int64, reason string) (bool, error)
	OpenQuestions(ctx context.Context, planID string) ([]store.Question, error)
	TransactionByID(ctx context.Context, planID, txnID string) (store.TransactionDetail, bool, error)
	CategoriesWithGroups(ctx context.Context, planID string) ([]store.CategoryWithGroup, error)
	Locks(ctx context.Context, planID string, txnIDs []string) (map[string][]store.HumanLock, error)
}

// Applier writes an answer to YNAB; *writer.Writer meets it.
type Applier interface {
	Apply(ctx context.Context, batch writer.Batch, actor, via string) ([]writer.Outcome, error)
}

// Messenger posts and edits messages in the channel; *Client meets it. text
// is the notification fallback; threadTS is empty for a top-level post.
type Messenger interface {
	ChannelID() string
	PostBlocks(ctx context.Context, threadTS, text string, blocks []slackapi.Block) (string, error)
	UpdateBlocks(ctx context.Context, ts, text string, blocks []slackapi.Block) error
}

// Questions asks questions in Slack and handles their answers. A click or
// thread reply is routed by the message it's on, and everything it acts on
// (kind, transaction, category ids) comes from the stored question, never from
// the payload: a button's value carries only the question id and an option
// index. ClaimAnswer picks the one winner of a race.
type Questions struct {
	st    QuestionStore
	apply Applier
	msg   Messenger
	log   *slog.Logger
	now   func() time.Time
}

// NewQuestions returns a question handler. apply runs categorize answers.
func NewQuestions(st QuestionStore, apply Applier, msg Messenger, log *slog.Logger) *Questions {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Questions{st: st, apply: apply, msg: msg, log: log, now: time.Now}
}

// Handlers routes thread replies and clicks to q, for the Client.
func (q *Questions) Handlers() Handlers {
	return Handlers{Message: q.HandleReply, Click: q.HandleClick}
}

// Ask posts pending question id and marks it open at the new message.
func (q *Questions) Ask(ctx context.Context, id int64) error {
	qu, ok, err := q.st.QuestionByID(ctx, id)
	if err != nil {
		return err
	}
	if !ok || qu.Status != store.QuestionPending {
		return store.ErrQuestionNotPending
	}
	txn, ok, err := q.st.TransactionByID(ctx, qu.PlanID, qu.SubjectID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("slack: ask: transaction not in mirror")
	}
	text, blocks := questionMessage(qu, txn, false)
	ts, err := q.msg.PostBlocks(ctx, "", text, blocks)
	if err != nil {
		return err
	}
	return q.st.MarkQuestionPosted(ctx, id, q.msg.ChannelID(), ts)
}

// HandleClick handles a button press or select choice on a question.
func (q *Questions) HandleClick(ctx context.Context, k Click) {
	qu, ok, err := q.st.QuestionByMessage(ctx, k.Channel, k.MessageTS)
	if err != nil {
		q.log.Error("slack question lookup failed", "err", err)
		return
	}
	if !ok {
		q.log.Debug("slack click on a non-question message")
		return
	}
	switch {
	case strings.HasPrefix(k.ActionID, actionPick), k.ActionID == actionSelect:
		idx, ok := parseChoice(k.Value, qu)
		if !ok {
			q.log.Warn("slack click rejected: bad question choice", "action", k.ActionID)
			return
		}
		q.answer(ctx, qu, qu.Options[idx].CategoryID, optionName(qu.Options[idx]), k.User)
	case k.ActionID == actionOther:
		if !sameQuestion(k.Value, qu) {
			q.log.Warn("slack click rejected: bad question id", "action", k.ActionID)
			return
		}
		if qu.Status != store.QuestionOpen {
			return
		}
		txn, ok, err := q.st.TransactionByID(ctx, qu.PlanID, qu.SubjectID)
		if err != nil || !ok {
			q.log.Error("slack other: transaction lookup failed", "found", ok, "err", err)
			return
		}
		text, blocks := questionMessage(qu, txn, true)
		if err := q.msg.UpdateBlocks(ctx, *qu.MessageTS, text, blocks); err != nil {
			q.log.Error("slack other: update failed", "err", err)
		}
	case k.ActionID == actionSplit:
		if !sameQuestion(k.Value, qu) {
			q.log.Warn("slack click rejected: bad question id", "action", k.ActionID)
			return
		}
		won, err := q.st.CloseQuestion(ctx, qu.ID, CloseSplit)
		if err != nil {
			q.log.Error("slack split: close failed", "err", err)
			return
		}
		if !won {
			return
		}
		q.finish(ctx, qu, "Split it in YNAB; finbot won't categorize this one. YNAB wasn't changed.", &k.User)
	default:
		q.log.Debug("slack click: unknown action", "action", k.ActionID)
	}
}

// HandleReply handles a thread reply under a question: an exact category name
// answers it, anything else gets a one-line pointer to the buttons.
func (q *Questions) HandleReply(ctx context.Context, m Message) {
	if m.ThreadTS == "" {
		return
	}
	qu, ok, err := q.st.QuestionByMessage(ctx, m.Channel, m.ThreadTS)
	if err != nil {
		q.log.Error("slack question lookup failed", "err", err)
		return
	}
	if !ok || qu.Status != store.QuestionOpen {
		return
	}
	id, name, ok, err := q.matchCategory(ctx, qu, m.Text)
	if err != nil {
		q.log.Error("slack reply: category lookup failed", "err", err)
		return
	}
	if !ok {
		_, err := q.msg.PostBlocks(ctx, m.ThreadTS,
			"I only understand an exact category name here. Use the buttons above.", nil)
		if err != nil {
			q.log.Error("slack reply: pointer failed", "err", err)
		}
		return
	}
	q.answer(ctx, qu, id, name, m.User)
}

// matchCategory finds the one category named exactly text (ignoring case and
// surrounding space): the question's candidates first, then the plan's
// visible categories. Two categories with that name match neither.
func (q *Questions) matchCategory(ctx context.Context, qu store.Question, text string) (id, name string, ok bool, err error) {
	want := strings.TrimSpace(unescapeSlack(text))
	if want == "" {
		return "", "", false, nil
	}
	for _, o := range qu.Options {
		if o.CategoryID != "" && (strings.EqualFold(o.CategoryName, want) || strings.EqualFold(o.Label, want)) {
			return o.CategoryID, optionName(o), true, nil
		}
	}
	cats, err := q.st.CategoriesWithGroups(ctx, qu.PlanID)
	if err != nil {
		return "", "", false, err
	}
	n := 0
	for _, c := range cats {
		if c.Deleted || c.Hidden || c.GroupDeleted || c.GroupHidden || !strings.EqualFold(c.Name, want) {
			continue
		}
		id, name = c.ID, c.Name
		n++
	}
	return id, name, n == 1, nil
}

// answer claims qu for user with categoryID and, if this caller won, acts on it
// by the stored kind and edits the message.
func (q *Questions) answer(ctx context.Context, qu store.Question, categoryID, name string, user store.User) {
	won, err := q.st.ClaimAnswer(ctx, qu.ID, categoryID, user.Label, q.now())
	if err != nil {
		q.log.Error("slack answer: claim failed", "err", err)
		return
	}
	if !won {
		q.log.Debug("slack answer: question already answered or closed")
		return
	}
	var result string
	switch qu.Kind {
	case store.QuestionDryRunCheck:
		result = fmt.Sprintf("Recorded %q (dry-run check). YNAB wasn't changed.", name)
	case store.QuestionCategorize:
		result = q.applyAnswer(ctx, qu, categoryID, name, user)
	default:
		result = fmt.Sprintf("Recorded %q. YNAB wasn't changed.", name)
	}
	q.finish(ctx, qu, result, &user)
}

// applyAnswer runs a categorize answer through the Applier and says what
// became of it.
func (q *Questions) applyAnswer(ctx context.Context, qu store.Question, categoryID, name string, user store.User) string {
	recorded := fmt.Sprintf("Recorded %q. YNAB wasn't changed", name)
	txn, ok, err := q.st.TransactionByID(ctx, qu.PlanID, qu.SubjectID)
	if err != nil || !ok {
		q.log.Error("slack answer: transaction lookup failed", "found", ok, "err", err)
		return recorded + ": the transaction isn't in the mirror."
	}
	out, err := q.apply.Apply(ctx, writer.Batch{PlanID: qu.PlanID, Writes: []writer.Write{{
		TxnID: qu.SubjectID, CategoryID: categoryID, Approve: true, DecidedCategoryID: txn.CategoryID,
	}}}, user.Label, store.ViaSlack)
	switch {
	case errors.Is(err, writer.ErrDryRun):
		return recorded + " (dry run)."
	case errors.Is(err, writer.ErrBeforeAutoSince):
		return recorded + " (dated before automatic writes start)."
	case errors.Is(err, writer.ErrUnknownOutcome):
		q.log.Warn("slack answer: write outcome unknown", "err", err)
		return fmt.Sprintf("Recorded %q. Whether YNAB took the change is unknown; finbot checks after the next sync.", name)
	case err != nil:
		q.log.Error("slack answer: apply failed", "err", err)
		return recorded + ": the write failed."
	case len(out) != 1:
		q.log.Error("slack answer: apply returned no outcome")
		return recorded + ": the write failed."
	}
	switch o := out[0]; o.Status {
	case writer.StatusDone:
		return fmt.Sprintf("Categorized as %q in YNAB.", name)
	case writer.StatusSkipped:
		return recorded + ": " + o.Reason + "."
	case writer.StatusUnknown:
		return fmt.Sprintf("Recorded %q. Whether YNAB took the change is unknown; finbot checks after the next sync.", name)
	default:
		return recorded + ": the write failed."
	}
}

// CloseStale closes planID's open questions whose transaction is now deleted
// (or gone from the mirror) or has a category lock (a person categorized it in
// YNAB), and edits each message. Call it after a sync. It returns how many it
// closed.
func (q *Questions) CloseStale(ctx context.Context, planID string) (int, error) {
	open, err := q.st.OpenQuestions(ctx, planID)
	if err != nil || len(open) == 0 {
		return 0, err
	}
	ids := make([]string, len(open))
	for i, qu := range open {
		ids[i] = qu.SubjectID
	}
	locks, err := q.st.Locks(ctx, planID, ids)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, qu := range open {
		txn, ok, err := q.st.TransactionByID(ctx, planID, qu.SubjectID)
		if err != nil {
			return closed, err
		}
		var reason, text string
		switch {
		case !ok || txn.Deleted:
			reason, text = CloseDeleted, "Closed: the transaction was deleted in YNAB."
		case hasCategoryLock(locks[qu.SubjectID]):
			reason, text = CloseLocked, "Closed: someone categorized this in YNAB."
		default:
			continue
		}
		won, err := q.st.CloseQuestion(ctx, qu.ID, reason)
		if err != nil {
			return closed, err
		}
		if !won {
			continue
		}
		closed++
		q.finish(ctx, qu, text, nil)
	}
	return closed, nil
}

func hasCategoryLock(ls []store.HumanLock) bool {
	for _, l := range ls {
		if l.Field == store.LockFieldCategory {
			return true
		}
	}
	return false
}

// Reasons the classify pipeline closes a question (classify.ReasonSuperseded
// and classify.ReasonSupersededByAuto), named here so slack needn't import
// classify.
const (
	reasonSuperseded       = "superseded"
	reasonSupersededByAuto = "superseded_by_auto"
)

// Closed edits the message of a question the pipeline closed with reason to
// its final state. It does nothing for a question that was never posted.
// classify.Asker.
func (q *Questions) Closed(ctx context.Context, qu store.Question, reason string) error {
	text := "Closed: no answer is needed any more."
	switch reason {
	case reasonSuperseded:
		text = "Closed: replaced by a newer question about this transaction."
	case reasonSupersededByAuto:
		text = "Closed: finbot now categorizes these automatically."
	}
	return q.finish(ctx, qu, text, nil)
}

// finish edits a question's message to its final state: the transaction (if
// the mirror still has it), the result and who answered. Errors are logged
// and returned.
func (q *Questions) finish(ctx context.Context, qu store.Question, result string, by *store.User) error {
	if qu.MessageTS == nil {
		return nil
	}
	var blocks []slackapi.Block
	if txn, ok, err := q.st.TransactionByID(ctx, qu.PlanID, qu.SubjectID); err == nil && ok {
		blocks = detailBlocks(txn)
	}
	elems := []slackapi.MixedElement{slackapi.NewTextBlockObject(slackapi.PlainTextType, result, false, false)}
	if by != nil {
		elems = append(elems, slackapi.NewTextBlockObject(slackapi.MarkdownType, "by "+who(*by), false, false))
	}
	blocks = append(blocks, slackapi.NewContextBlock("q_result", elems...))
	err := q.msg.UpdateBlocks(ctx, *qu.MessageTS, result, blocks)
	if err != nil {
		q.log.Error("slack question update failed", "err", err)
	}
	return err
}

var slackUserID = regexp.MustCompile(`^[UW][A-Z0-9]+$`)

// who names the person who answered: a Slack mention, or their label.
func who(u store.User) string {
	if slackUserID.MatchString(u.SlackUserID) {
		return "<@" + u.SlackUserID + ">"
	}
	return u.Label
}

// questionMessage renders an open question. withSelect swaps the "other…"
// button for a select of every candidate.
func questionMessage(qu store.Question, txn store.TransactionDetail, withSelect bool) (string, []slackapi.Block) {
	prompt := "Which category is this?"
	if qu.Kind == store.QuestionDryRunCheck {
		prompt = "Dry-run check: which category is right? Answering won't change YNAB."
	}
	blocks := []slackapi.Block{
		slackapi.NewSectionBlock(slackapi.NewTextBlockObject(slackapi.PlainTextType, prompt, false, false), nil, nil),
	}
	blocks = append(blocks, detailBlocks(txn)...)

	qid := strconv.FormatInt(qu.ID, 10)
	var elems []slackapi.BlockElement
	var all []*slackapi.OptionBlockObject
	for i, o := range qu.Options {
		if o.CategoryID == "" {
			continue
		}
		val := qid + ":" + strconv.Itoa(i)
		if len(elems) < topN {
			elems = append(elems, slackapi.NewButtonBlockElement(actionPick+strconv.Itoa(len(elems)), val,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, clip(optionName(o)), false, false)))
		}
		if len(all) < maxSelect {
			all = append(all, slackapi.NewOptionBlockObject(val,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, clip(optionName(o)), false, false), nil))
		}
	}
	if len(all) > len(elems) {
		if withSelect {
			elems = append(elems, slackapi.NewOptionsSelectBlockElement(slackapi.OptTypeStatic,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "Pick a category", false, false), actionSelect, all...))
		} else {
			elems = append(elems, slackapi.NewButtonBlockElement(actionOther, qid,
				slackapi.NewTextBlockObject(slackapi.PlainTextType, "other…", false, false)))
		}
	}
	elems = append(elems, slackapi.NewButtonBlockElement(actionSplit, qid,
		slackapi.NewTextBlockObject(slackapi.PlainTextType, "split", false, false)))
	blocks = append(blocks, slackapi.NewActionBlock("q_actions", elems...))

	return "finbot: " + prompt, blocks
}

// detailBlocks shows the transaction. Every text from YNAB (payee, account,
// memo) is plain_text, so it can't format, link or mention anyone.
func detailBlocks(txn store.TransactionDetail) []slackapi.Block {
	payee := "(no payee)"
	switch {
	case txn.PayeeName != nil && *txn.PayeeName != "":
		payee = *txn.PayeeName
	case txn.ImportPayeeName != nil && *txn.ImportPayeeName != "":
		payee = *txn.ImportPayeeName
	}
	account := "(unknown account)"
	if txn.AccountName != nil {
		account = *txn.AccountName
	}
	plain := func(s string) *slackapi.TextBlockObject {
		return slackapi.NewTextBlockObject(slackapi.PlainTextType, s, false, false)
	}
	out := []slackapi.Block{slackapi.NewSectionBlock(nil, []*slackapi.TextBlockObject{
		plain("Payee: " + payee),
		plain("Amount: " + formatAmount(txn.Amount)),
		plain("Date: " + txn.Date),
		plain("Account: " + account),
	}, nil)}
	if txn.Memo != nil && strings.TrimSpace(*txn.Memo) != "" {
		out = append(out, slackapi.NewContextBlock("", plain("Memo: "+*txn.Memo)))
	}
	return out
}

// formatAmount renders milliunits as dollars: -12340 → "-$12.34".
func formatAmount(milli int64) string {
	sign := ""
	if milli < 0 {
		sign, milli = "-", -milli
	}
	cents := (milli + 5) / 10
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

func optionName(o store.QuestionOption) string {
	if o.Label != "" {
		return o.Label
	}
	return o.CategoryName
}

// clip shortens s to Block Kit's 75-character button text limit.
func clip(s string) string {
	r := []rune(s)
	if len(r) <= maxBtnText {
		return s
	}
	return string(r[:maxBtnText-1]) + "…"
}

// parseChoice reads a "<question id>:<option index>" value and returns the
// index if it names a category option of qu.
func parseChoice(v string, qu store.Question) (int, bool) {
	id, idx, ok := strings.Cut(v, ":")
	if !ok || !sameQuestion(id, qu) {
		return 0, false
	}
	i, err := strconv.Atoi(idx)
	if err != nil || i < 0 || i >= len(qu.Options) || qu.Options[i].CategoryID == "" || idx != strconv.Itoa(i) {
		return 0, false
	}
	return i, true
}

func sameQuestion(v string, qu store.Question) bool {
	return v == strconv.FormatInt(qu.ID, 10)
}

// unescapeSlack undoes Slack's escaping of &, < and > in message text.
func unescapeSlack(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

var (
	_ QuestionStore = (*store.DB)(nil)
	_ Applier       = (*writer.Writer)(nil)
	_ Messenger     = (*Client)(nil)
)
