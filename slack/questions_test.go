package slack

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
)

const (
	qPlan    = "plan-0000-uuid"
	qAcct    = "acct-1111-uuid"
	qPayee   = "payee-2222-uuid"
	qGroup   = "group-3333-uuid"
	qTxn     = "txn-4444-uuid"
	qTS      = "500.1"
	catGroc  = "cat-groc-uuid"
	catGas   = "cat-gas-uuid"
	catDine  = "cat-dine-uuid"
	catFun   = "cat-fun-uuid"
	catHome  = "cat-home-uuid"
	catOther = "cat-other-uuid" // in the plan, not among the candidates
)

var allIDs = []string{qPlan, qAcct, qPayee, qGroup, qTxn, catGroc, catGas, catDine, catFun, catHome, catOther}

// sent is one Post or Update, decoded.
type sent struct {
	ts     string // update target, or thread ts of a post
	text   string
	blocks string // raw JSON
}

type fakeMessenger struct {
	mu      sync.Mutex
	n       int
	posts   []sent
	updates []sent
}

func decode(t *testing.T, ts, text string, blocks []slackapi.Block) sent {
	t.Helper()
	body, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return sent{ts: ts, text: text, blocks: string(body)}
}

type recMessenger struct {
	t *testing.T
	fakeMessenger
}

func (m *recMessenger) ChannelID() string { return testChannelID }
func (m *recMessenger) PostBlocks(_ context.Context, threadTS, text string, blocks []slackapi.Block) (string, error) {
	s := decode(m.t, threadTS, text, blocks)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.posts = append(m.posts, s)
	if s.ts != "" {
		return "900.1", nil // a thread reply
	}
	m.n++
	return "50" + strconv.Itoa(m.n) + ".1", nil
}
func (m *recMessenger) UpdateBlocks(_ context.Context, ts, text string, blocks []slackapi.Block) error {
	s := decode(m.t, ts, text, blocks)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updates = append(m.updates, s)
	return nil
}
func (m *recMessenger) lastUpdate(t *testing.T) sent {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.updates) == 0 {
		t.Fatal("no update")
	}
	return m.updates[len(m.updates)-1]
}

type fakeApplier struct {
	calls atomic.Int32
	out   writer.Status
	err   error
	got   []writer.Batch
	mu    sync.Mutex
}

func (a *fakeApplier) Apply(_ context.Context, b writer.Batch, actor, via string) ([]writer.Outcome, error) {
	a.calls.Add(1)
	a.mu.Lock()
	a.got = append(a.got, b)
	a.mu.Unlock()
	time.Sleep(20 * time.Millisecond) // widen any race
	if a.err != nil && !errors.Is(a.err, writer.ErrBeforeAutoSince) {
		return nil, a.err
	}
	st := a.out
	if st == "" {
		st = writer.StatusDone
	}
	return []writer.Outcome{{TxnID: b.Writes[0].TxnID, Status: st}}, a.err
}

type qenv struct {
	db    *store.DB
	path  string
	msg   *recMessenger
	apply *fakeApplier
	q     *Questions
}

func seedMirror(t *testing.T, db *store.DB, payee, memo string) {
	t.Helper()
	err := db.Update(context.Background(), func(tx *store.Tx) error {
		if err := tx.UpsertAccounts(qPlan, []store.Account{{ID: qAcct, Name: "Checking", Type: "checking", OnBudget: true}}); err != nil {
			return err
		}
		if err := tx.UpsertPayees(qPlan, []store.Payee{{ID: qPayee, Name: payee}}); err != nil {
			return err
		}
		if err := tx.UpsertCategoryGroups(qPlan, []store.CategoryGroup{{ID: qGroup, Name: "Everyday"}}); err != nil {
			return err
		}
		var cats []store.Category
		for id, name := range map[string]string{catGroc: "Groceries", catGas: "Gas", catDine: "Dining Out",
			catFun: "Fun Money", catHome: "Home & Garden", catOther: "Gifts"} {
			cats = append(cats, store.Category{ID: id, CategoryGroupID: qGroup, Name: name})
		}
		if err := tx.UpsertCategories(qPlan, cats); err != nil {
			return err
		}
		return tx.UpsertTransactions(qPlan, []store.Transaction{{ID: qTxn, Date: "2026-11-10", Amount: -12340,
			Memo: &memo, Cleared: "cleared", AccountID: qAcct, PayeeID: ptr(qPayee)}}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func putTxn(t *testing.T, db *store.DB, rows ...store.Transaction) {
	t.Helper()
	if err := db.Update(context.Background(), func(tx *store.Tx) error {
		return tx.UpsertTransactions(qPlan, rows, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
}

func ptr(s string) *string { return &s }

func candidates() []store.QuestionOption {
	return []store.QuestionOption{
		{Label: "Groceries", CategoryID: catGroc, CategoryName: "Groceries"},
		{Label: "Gas", CategoryID: catGas, CategoryName: "Gas"},
		{Label: "Dining Out", CategoryID: catDine, CategoryName: "Dining Out"},
		{Label: "Fun Money", CategoryID: catFun, CategoryName: "Fun Money"},
		{Label: "Home & Garden", CategoryID: catHome, CategoryName: "Home & Garden"},
	}
}

func newQEnv(t *testing.T, kind, payee string) (*qenv, int64) {
	t.Helper()
	db, path := newStore(t)
	seedMirror(t, db, payee, "weekly shop")
	e := &qenv{db: db, path: path, msg: &recMessenger{t: t}, apply: &fakeApplier{}}
	e.q = NewQuestions(db, e.apply, e.msg, nil)
	id := e.ask(t, kind, qTxn)
	return e, id
}

// ask inserts a pending question about txn and posts it.
func (e *qenv) ask(t *testing.T, kind, txn string) int64 {
	t.Helper()
	id, err := e.db.InsertQuestion(context.Background(), store.Question{PlanID: qPlan, Kind: kind, SubjectID: txn,
		Options: candidates()})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.q.Ask(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *qenv) question(t *testing.T, id int64) store.Question {
	t.Helper()
	q, ok, err := e.db.QuestionByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("question %d: ok=%v err=%v", id, ok, err)
	}
	return q
}

func click(user store.User, ts, action, value string) Click {
	return Click{User: user, Channel: testChannelID, MessageTS: ts, ActionTS: "1.1", ActionID: action, Value: value}
}

var (
	brendan = store.User{SlackUserID: userBrendan, Label: store.UserBrendan}
	wife    = store.User{SlackUserID: userWife, Label: store.UserWife}
)

// blockValues pulls every button and option value out of a blocks payload.
func blockValues(t *testing.T, blocks string) []string {
	t.Helper()
	var bs []map[string]any
	if err := json.Unmarshal([]byte(blocks), &bs); err != nil {
		t.Fatal(err)
	}
	var vals []string
	for _, b := range bs {
		els, _ := b["elements"].([]any)
		for _, el := range els {
			m, _ := el.(map[string]any)
			if v, ok := m["value"].(string); ok {
				vals = append(vals, v)
			}
			opts, _ := m["options"].([]any)
			for _, o := range opts {
				if v, ok := o.(map[string]any)["value"].(string); ok {
					vals = append(vals, v)
				}
			}
		}
	}
	return vals
}

func TestQuestionMessageCarriesNoYNABID(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	if len(e.msg.posts) != 1 {
		t.Fatalf("posts = %d", len(e.msg.posts))
	}
	q := e.question(t, id)
	if q.Status != store.QuestionOpen || *q.MessageTS != "501.1" || *q.ChannelID != testChannelID {
		t.Fatalf("question = %+v", q)
	}
	// Then open the select so its options are checked too.
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionOther, strconv.FormatInt(id, 10)))
	for _, s := range []sent{e.msg.posts[0], e.msg.lastUpdate(t)} {
		vals := blockValues(t, s.blocks)
		if len(vals) == 0 {
			t.Fatal("no values")
		}
		for _, v := range vals {
			if v != strconv.FormatInt(id, 10) && !strings.HasPrefix(v, strconv.FormatInt(id, 10)+":") {
				t.Errorf("value %q is not <question id>[:<index>]", v)
			}
		}
		for _, yid := range allIDs {
			if strings.Contains(s.blocks, yid) || strings.Contains(s.text, yid) {
				t.Errorf("message carries YNAB id %q", yid)
			}
		}
	}
	for _, want := range []string{"Costco", "-$12.34", "2026-11-10", "Checking", "weekly shop", "Groceries", "other…", "split"} {
		if !strings.Contains(e.msg.posts[0].blocks, want) {
			t.Errorf("question lacks %q", want)
		}
	}
}

func TestPickButtonCategorizes(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	e.q.HandleClick(context.Background(), click(wife, "501.1", actionPick+"1", strconv.FormatInt(id, 10)+":1"))
	if e.apply.calls.Load() != 1 {
		t.Fatalf("apply calls = %d", e.apply.calls.Load())
	}
	b := e.apply.got[0]
	if b.PlanID != qPlan || len(b.Writes) != 1 || b.Writes[0].TxnID != qTxn || b.Writes[0].CategoryID != catGas ||
		!b.Writes[0].Approve || b.Writes[0].DecidedCategoryID != nil {
		t.Fatalf("batch = %+v", b)
	}
	q := e.question(t, id)
	if q.Status != store.QuestionAnswered || *q.Answer != catGas || *q.AnsweredBy != store.UserWife {
		t.Fatalf("question = %+v", q)
	}
	u := e.msg.lastUpdate(t)
	if u.ts != "501.1" || !strings.Contains(u.blocks, `Categorized as \"Gas\" in YNAB`) ||
		!strings.Contains(u.blocks, "by wife") || strings.Contains(u.blocks, actionPick) {
		t.Fatalf("update = %+v", u)
	}
}

func TestTamperedChoiceRejected(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	other, err := e.db.InsertQuestion(context.Background(), store.Question{PlanID: qPlan, Kind: store.QuestionCategorize,
		SubjectID: "txn-other", Options: candidates()})
	if err != nil {
		t.Fatal(err)
	}
	qid := strconv.FormatInt(id, 10)
	for _, v := range []string{qid + ":5", qid + ":-1", qid + ":99", qid + ":x", qid + ":01", qid,
		strconv.FormatInt(other, 10) + ":0", "txn:" + qTxn + "|cat:" + catGroc, ""} {
		e.q.HandleClick(context.Background(), click(brendan, "501.1", actionPick+"0", v))
		e.q.HandleClick(context.Background(), click(brendan, "501.1", actionSelect, v))
	}
	// A split or other click naming another question is rejected too.
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionSplit, strconv.FormatInt(other, 10)))
	if n := e.apply.calls.Load(); n != 0 {
		t.Fatalf("apply calls = %d", n)
	}
	if q := e.question(t, id); q.Status != store.QuestionOpen {
		t.Fatalf("status = %s", q.Status)
	}
	if len(e.msg.updates) != 0 {
		t.Fatalf("updates = %d", len(e.msg.updates))
	}
}

func TestConcurrentClicksApplyOnce(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	qid := strconv.FormatInt(id, 10)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, u := range []store.User{brendan, wife, brendan, wife} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			e.q.HandleClick(context.Background(), click(u, "501.1", actionPick+strconv.Itoa(i%3), qid+":"+strconv.Itoa(i%3)))
		}()
	}
	close(start)
	wg.Wait()
	if n := e.apply.calls.Load(); n != 1 {
		t.Fatalf("apply calls = %d", n)
	}
	if len(e.msg.updates) != 1 {
		t.Fatalf("updates = %d", len(e.msg.updates))
	}
}

func TestRoutingAfterRestart(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	e.db.Close()
	db := openStore(t, e.path)
	t.Cleanup(func() { db.Close() })
	apply := &fakeApplier{}
	msg := &recMessenger{t: t}
	q := NewQuestions(db, apply, msg, nil)
	q.HandleClick(context.Background(), click(brendan, "501.1", actionPick+"0", strconv.FormatInt(id, 10)+":0"))
	if apply.calls.Load() != 1 || apply.got[0].Writes[0].CategoryID != catGroc {
		t.Fatalf("apply = %d %+v", apply.calls.Load(), apply.got)
	}
	got, _, err := db.QuestionByID(context.Background(), id)
	if err != nil || got.Status != store.QuestionAnswered {
		t.Fatalf("question = %+v, %v", got, err)
	}
}

func TestOtherSwapsInSelect(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	qid := strconv.FormatInt(id, 10)
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionOther, qid))
	u := e.msg.lastUpdate(t)
	if !strings.Contains(u.blocks, `"static_select"`) || strings.Contains(u.blocks, actionOther) ||
		!strings.Contains(u.blocks, "Fun Money") || !strings.Contains(u.blocks, `Home \u0026 Garden`) {
		t.Fatalf("update = %s", u.blocks)
	}
	if q := e.question(t, id); q.Status != store.QuestionOpen {
		t.Fatal("other answered the question")
	}
	// Choosing the 5th candidate (index 4) from the select answers it.
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionSelect, qid+":4"))
	if e.apply.calls.Load() != 1 || e.apply.got[0].Writes[0].CategoryID != catHome {
		t.Fatalf("apply = %+v", e.apply.got)
	}
}

func TestSplitClosesQuestion(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	qid := strconv.FormatInt(id, 10)
	e.q.HandleClick(context.Background(), click(wife, "501.1", actionSplit, qid))
	q := e.question(t, id)
	if q.Status != store.QuestionClosed || *q.CloseReason != CloseSplit {
		t.Fatalf("question = %+v", q)
	}
	u := e.msg.lastUpdate(t)
	if !strings.Contains(u.blocks, "Split it in YNAB") || !strings.Contains(u.blocks, "by wife") {
		t.Fatalf("update = %s", u.blocks)
	}
	// A late pick does nothing.
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionPick+"0", qid+":0"))
	if e.apply.calls.Load() != 0 || len(e.msg.updates) != 1 {
		t.Fatal("late pick acted on a closed question")
	}
}

func TestExactNameReplyAnswers(t *testing.T) {
	for _, tc := range []struct{ text, cat string }{
		{"  fun money ", catFun},       // a candidate, any case
		{"Home &amp; Garden", catHome}, // Slack escapes &
		{"Gifts", catOther},            // a plan category that wasn't offered
	} {
		e, id := newQEnv(t, store.QuestionCategorize, "Costco")
		e.q.HandleReply(context.Background(), Message{User: brendan, Channel: testChannelID, TS: "600.1",
			ThreadTS: "501.1", Text: tc.text})
		if e.apply.calls.Load() != 1 || e.apply.got[0].Writes[0].CategoryID != tc.cat {
			t.Fatalf("%q: apply = %+v", tc.text, e.apply.got)
		}
		if q := e.question(t, id); q.Status != store.QuestionAnswered || *q.Answer != tc.cat {
			t.Fatalf("%q: question = %+v", tc.text, q)
		}
		if len(e.msg.posts) != 1 {
			t.Fatalf("%q: extra posts %d", tc.text, len(e.msg.posts))
		}
	}
}

func TestNonMatchingReplyGetsPointer(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	e.q.HandleReply(context.Background(), Message{User: brendan, Channel: testChannelID, TS: "600.1",
		ThreadTS: "501.1", Text: "groceries I think"})
	if e.apply.calls.Load() != 0 {
		t.Fatal("applied")
	}
	if q := e.question(t, id); q.Status != store.QuestionOpen {
		t.Fatalf("status = %s", q.Status)
	}
	if len(e.msg.posts) != 2 || e.msg.posts[1].ts != "501.1" || !strings.Contains(e.msg.posts[1].text, "Use the buttons") ||
		strings.Count(e.msg.posts[1].text, "\n") != 0 {
		t.Fatalf("posts = %+v", e.msg.posts)
	}
	// Top-level messages and replies to other threads are ignored.
	e.q.HandleReply(context.Background(), Message{User: brendan, Channel: testChannelID, TS: "601.1", Text: "Gas"})
	e.q.HandleReply(context.Background(), Message{User: brendan, Channel: testChannelID, TS: "602.1", ThreadTS: "1.1", Text: "x"})
	if len(e.msg.posts) != 2 || e.apply.calls.Load() != 0 {
		t.Fatal("acted on a non-question message")
	}
}

func TestDryRunCheckNeverApplies(t *testing.T) {
	e, id := newQEnv(t, store.QuestionDryRunCheck, "Costco")
	// The fake would succeed: auto is in effect. The kind alone decides.
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionPick+"0", strconv.FormatInt(id, 10)+":0"))
	if n := e.apply.calls.Load(); n != 0 {
		t.Fatalf("apply calls = %d", n)
	}
	q := e.question(t, id)
	if q.Status != store.QuestionAnswered || *q.Answer != catGroc || *q.AnsweredBy != store.UserBrendan {
		t.Fatalf("question = %+v", q)
	}
	if u := e.msg.lastUpdate(t); !strings.Contains(u.blocks, "YNAB wasn't changed") {
		t.Fatalf("update = %s", u.blocks)
	}
	// A thread reply on a dry_run_check question doesn't apply either.
	putTxn(t, e.db, store.Transaction{ID: "txn-other", Date: "2026-11-10", Amount: -1000, Cleared: "cleared", AccountID: qAcct})
	id2 := e.ask(t, store.QuestionDryRunCheck, "txn-other")
	e.q.HandleReply(context.Background(), Message{User: wife, Channel: testChannelID, TS: "700.1", ThreadTS: "502.1", Text: "Gas"})
	if q := e.question(t, id2); q.Status != store.QuestionAnswered || e.apply.calls.Load() != 0 {
		t.Fatalf("reply: status %s, apply %d", q.Status, e.apply.calls.Load())
	}
}

func TestCategorizeGatedAnswerIsRecorded(t *testing.T) {
	for _, err := range []error{writer.ErrDryRun, writer.ErrBeforeAutoSince} {
		e, id := newQEnv(t, store.QuestionCategorize, "Costco")
		e.apply.err = err
		if errors.Is(err, writer.ErrBeforeAutoSince) {
			e.apply.out = writer.StatusSkipped
		}
		e.q.HandleClick(context.Background(), click(brendan, "501.1", actionPick+"2", strconv.FormatInt(id, 10)+":2"))
		if e.apply.calls.Load() != 1 {
			t.Fatalf("%v: apply calls = %d", err, e.apply.calls.Load())
		}
		if q := e.question(t, id); q.Status != store.QuestionAnswered || *q.Answer != catDine {
			t.Fatalf("%v: question = %+v", err, q)
		}
		if u := e.msg.lastUpdate(t); !strings.Contains(u.blocks, "YNAB wasn't changed") {
			t.Fatalf("%v: update = %s", err, u.blocks)
		}
	}
}

func TestPayeeRendersInert(t *testing.T) {
	evil := "<!channel> *bold* <@U123|x> <https://evil|click>"
	db, _ := newStore(t)
	seedMirror(t, db, evil, "_memo_ <!here>")
	msg := &recMessenger{t: t}
	q := NewQuestions(db, &fakeApplier{}, msg, nil)
	id, err := db.InsertQuestion(context.Background(), store.Question{PlanID: qPlan, Kind: store.QuestionCategorize,
		SubjectID: qTxn, Options: candidates()})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Ask(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	s := msg.posts[0]
	if strings.Contains(s.text, "<!") || strings.Contains(s.text, "*bold*") {
		t.Fatalf("fallback text = %q", s.text)
	}
	// The Client sends the fallback escaped and with mrkdwn off.
	_, v, err := slackapi.UnsafeApplyMsgOptions("", testChannelID, "", blockOptions("", evil, nil)...)
	if err != nil {
		t.Fatal(err)
	}
	if v.Get("mrkdwn") != "false" || strings.Contains(v.Get("text"), "<!channel>") {
		t.Fatalf("client options = %v", v)
	}
	var bs []map[string]any
	if err := json.Unmarshal([]byte(s.blocks), &bs); err != nil {
		t.Fatal(err)
	}
	found := 0
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if txt, ok := x["text"].(string); ok && (strings.Contains(txt, "<!channel>") || strings.Contains(txt, "<!here>")) {
				found++
				if x["type"] != slackapi.PlainTextType {
					t.Errorf("payee or memo in a %v text object", x["type"])
				}
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	for _, b := range bs {
		walk(b)
	}
	if found != 2 {
		t.Fatalf("payee and memo found in %d text objects, want 2: %s", found, s.blocks)
	}
}

func TestCloseStaleClosesLockedAndDeletedOnly(t *testing.T) {
	e, untouched := newQEnv(t, store.QuestionCategorize, "Costco")
	ctx := context.Background()
	err := e.db.Update(ctx, func(tx *store.Tx) error {
		var rows []store.Transaction
		for _, id := range []string{"txn-del", "txn-lock", "txn-memo", "txn-guess"} {
			rows = append(rows, store.Transaction{ID: id, Date: "2026-11-10", Amount: -1000, Cleared: "cleared", AccountID: qAcct})
		}
		return tx.UpsertTransactions(qPlan, rows, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	del := e.ask(t, store.QuestionCategorize, "txn-del")
	locked := e.ask(t, store.QuestionDryRunCheck, "txn-lock")
	memo := e.ask(t, store.QuestionCategorize, "txn-memo")   // locked, but not its category
	guess := e.ask(t, store.QuestionCategorize, "txn-guess") // YNAB guessed a category, unapproved: no lock
	gone, err := e.db.InsertQuestion(ctx, store.Question{PlanID: qPlan, Kind: store.QuestionCategorize,
		SubjectID: "txn-never-mirrored", ChannelID: ptr(testChannelID), MessageTS: ptr("599.1"), Options: candidates()})
	if err != nil {
		t.Fatal(err)
	}
	err = e.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertTransactions(qPlan, []store.Transaction{
			{ID: "txn-del", Date: "2026-11-10", Amount: -1000, Cleared: "cleared", AccountID: qAcct, Deleted: true},
			{ID: "txn-guess", Date: "2026-11-10", Amount: -1000, Cleared: "cleared", AccountID: qAcct, CategoryID: ptr(catGas)},
		}, time.Now()); err != nil {
			return err
		}
		for _, l := range []store.HumanLock{
			{PlanID: qPlan, TxnID: "txn-lock", Field: store.LockFieldCategory, Source: "first_touch"},
			{PlanID: qPlan, TxnID: "txn-memo", Field: "memo", Source: "ynab_edit"},
		} {
			if _, err := tx.Lock(l); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := len(e.msg.updates)
	n, err := e.q.CloseStale(ctx, qPlan)
	if err != nil || n != 3 {
		t.Fatalf("closed %d, %v", n, err)
	}
	for id, reason := range map[int64]string{del: CloseDeleted, gone: CloseDeleted, locked: CloseLocked} {
		if q := e.question(t, id); q.Status != store.QuestionClosed || *q.CloseReason != reason {
			t.Errorf("question %d = %s %v, want closed %s", id, q.Status, q.CloseReason, reason)
		}
	}
	for _, id := range []int64{untouched, memo, guess} {
		if q := e.question(t, id); q.Status != store.QuestionOpen {
			t.Errorf("question %d = %s, want open", id, q.Status)
		}
	}
	if got := len(e.msg.updates) - before; got != 3 {
		t.Fatalf("updates = %d", got)
	}
	// Run again: nothing more to close.
	if n, err := e.q.CloseStale(ctx, qPlan); err != nil || n != 0 {
		t.Fatalf("second run closed %d, %v", n, err)
	}
}

func TestWhoMentionsOnlyARealSlackID(t *testing.T) {
	if got := who(store.User{SlackUserID: "U0ABC123", Label: store.UserWife}); got != "<@U0ABC123>" {
		t.Fatalf("who = %q", got)
	}
	if got := who(store.User{SlackUserID: "U1>|<!channel", Label: store.UserWife}); got != store.UserWife {
		t.Fatalf("who = %q", got)
	}
}

func TestClosedEditsPostedQuestion(t *testing.T) {
	e, id := newQEnv(t, store.QuestionCategorize, "Costco")
	ctx := context.Background()
	for reason, want := range map[string]string{
		"superseded":         "replaced by a newer question",
		"superseded_by_auto": "categorizes these automatically",
		"other":              "no answer is needed",
	} {
		if err := e.q.Closed(ctx, e.question(t, id), reason); err != nil {
			t.Fatal(err)
		}
		if u := e.msg.lastUpdate(t); !strings.Contains(u.text, want) || u.ts != "501.1" {
			t.Errorf("%s: update %q at %q", reason, u.text, u.ts)
		}
	}
	// Never posted: no edit.
	before := len(e.msg.updates)
	if err := e.q.Closed(ctx, store.Question{PlanID: qPlan, SubjectID: qTxn}, "superseded"); err != nil {
		t.Fatal(err)
	}
	if len(e.msg.updates) != before {
		t.Error("edited a question that was never posted")
	}
}
