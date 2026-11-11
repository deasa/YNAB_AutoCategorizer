package store

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func TestMigration0008OnExisting0007DB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "finbot.db")
	w, err := openPool(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	upTo7 := fstest.MapFS{}
	for _, name := range []string{"0001_mirror", "0002_plan_scan", "0003_status_message", "0004_plan_scan_keep_verdict",
		"0005_oauth", "0006_decisions", "0007_write_log_locks"} {
		body, err := fs.ReadFile(migrationFS, "migrations/"+name+".sql")
		if err != nil {
			t.Fatal(err)
		}
		upTo7["migrations/"+name+".sql"] = &fstest.MapFile{Data: body}
	}
	if err := migrateFS(ctx, w, upTo7); err != nil {
		t.Fatalf("apply through 0007: %v", err)
	}
	if _, err := w.ExecContext(ctx, `INSERT INTO sync_state VALUES ('p', 7, '2026-10-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	w.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open on a 0007 DB: %v", err)
	}
	defer db.Close()
	if n := count(t, db, `SELECT count(*) FROM schema_migrations WHERE version = 8`); n != 1 {
		t.Errorf("0008 rows in schema_migrations = %d, want 1", n)
	}
	for _, table := range []string{"questions", "users", "slack_events", "alerts", "import_state", "alert_cursor"} {
		if n := count(t, db, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s holds %d rows, want 0", table, n)
		}
	}
	if st, ok, err := db.GetSyncState(ctx, "p"); err != nil || !ok || st.ServerKnowledge != 7 {
		t.Errorf("pre-existing sync_state lost: %+v %v %v", st, ok, err)
	}
}

func catQuestion(subject string) Question {
	return Question{
		PlanID: plan, Kind: QuestionCategorize, SubjectID: subject, CreatedAt: t0,
		Options: []QuestionOption{
			{Label: "Groceries", CategoryID: "groc", CategoryName: "Groceries"},
			{Label: "other…"},
		},
	}
}

func insertQuestion(t *testing.T, db *DB, q Question) int64 {
	t.Helper()
	id, err := db.InsertQuestion(context.Background(), q)
	if err != nil {
		t.Fatalf("InsertQuestion: %v", err)
	}
	return id
}

func TestQuestionByMessageSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	db, path := openTest(t)
	q := catQuestion("txn-1")
	q.ChannelID, q.MessageTS = ptr("C1"), ptr("1730000000.000100")
	id := insertQuestion(t, db, q)
	db.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, ok, err := db.QuestionByMessage(ctx, "C1", "1730000000.000100")
	if err != nil || !ok {
		t.Fatalf("QuestionByMessage after reopen: ok=%v err=%v", ok, err)
	}
	if got.ID != id || got.Status != QuestionOpen || got.SubjectID != "txn-1" || got.Kind != QuestionCategorize ||
		!got.CreatedAt.Equal(t0) || len(got.Options) != 2 || got.Options[0] != q.Options[0] || got.Options[1] != q.Options[1] {
		t.Errorf("round trip = %+v", got)
	}
	if _, ok, _ := db.QuestionByMessage(ctx, "C1", "other-ts"); ok {
		t.Error("unknown message ts found a question")
	}
}

func TestQuestionMessageKeyIsUnique(t *testing.T) {
	db, _ := openTest(t)
	a, b := catQuestion("txn-1"), catQuestion("txn-2")
	a.ChannelID, a.MessageTS = ptr("C1"), ptr("1.1")
	b.ChannelID, b.MessageTS = ptr("C1"), ptr("1.1")
	insertQuestion(t, db, a)
	if _, err := db.InsertQuestion(context.Background(), b); err == nil {
		t.Error("second question on the same message was stored")
	}
}

func TestPendingQuestionPostedThenAnswered(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	id := insertQuestion(t, db, catQuestion("txn-1"))
	q, _, _ := db.QuestionByID(ctx, id)
	if q.Status != QuestionPending || q.ChannelID != nil {
		t.Fatalf("new question = %+v, want pending without a message", q)
	}
	if won, err := db.ClaimAnswer(ctx, id, "groc", UserBrendan, t0); err != nil || won {
		t.Errorf("ClaimAnswer on pending = %v %v, want false", won, err)
	}
	if err := db.MarkQuestionPosted(ctx, id, "C1", "2.2"); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkQuestionPosted(ctx, id, "C1", "3.3"); !errors.Is(err, ErrQuestionNotPending) {
		t.Errorf("second MarkQuestionPosted = %v, want ErrQuestionNotPending", err)
	}
	at := t0.Add(time.Hour)
	if won, err := db.ClaimAnswer(ctx, id, "groc", UserWife, at); err != nil || !won {
		t.Fatalf("ClaimAnswer = %v %v, want true", won, err)
	}
	q, _, _ = db.QuestionByMessage(ctx, "C1", "2.2")
	if q.Status != QuestionAnswered || *q.Answer != "groc" || *q.AnsweredBy != UserWife || !q.AnsweredAt.Equal(at) {
		t.Errorf("answered question = %+v", q)
	}
	if closed, err := db.CloseQuestion(ctx, id, "superseded"); err != nil || closed {
		t.Errorf("CloseQuestion on answered = %v %v, want false", closed, err)
	}
}

func TestClaimAnswerRejectsUnknownUser(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	q := catQuestion("txn-1")
	q.ChannelID, q.MessageTS = ptr("C1"), ptr("1.1")
	id := insertQuestion(t, db, q)
	if _, err := db.ClaimAnswer(ctx, id, "groc", "U123", t0); err == nil {
		t.Error("answer by a non-label was stored")
	}
	if got, _, _ := db.QuestionByID(ctx, id); got.Status != QuestionOpen {
		t.Errorf("status = %s, want open", got.Status)
	}
}

func TestConcurrentClaimAnswerExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	for round := 0; round < 20; round++ {
		q := catQuestion("txn-race")
		q.ChannelID, q.MessageTS = ptr("C1"), ptr("ts-"+string(rune('a'+round)))
		id := insertQuestion(t, db, q)
		var (
			wg   sync.WaitGroup
			wins [2]bool
			errs [2]error
		)
		start := make(chan struct{})
		for i, who := range []string{UserBrendan, UserWife} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				wins[i], errs[i] = db.ClaimAnswer(ctx, id, "answer-"+who, who, t0)
			}()
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("round %d errors: %v %v", round, errs[0], errs[1])
		}
		if wins[0] == wins[1] {
			t.Fatalf("round %d wins = %v, want exactly one", round, wins)
		}
		got, _, _ := db.QuestionByID(ctx, id)
		winner := UserBrendan
		if wins[1] {
			winner = UserWife
		}
		if *got.AnsweredBy != winner || *got.Answer != "answer-"+winner {
			t.Fatalf("round %d stored %s/%s, winner %s", round, *got.AnsweredBy, *got.Answer, winner)
		}
		if _, err := db.CloseQuestion(ctx, id, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCountsListsAndCloseQuestions(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	open1 := catQuestion("txn-1")
	open1.ChannelID, open1.MessageTS = ptr("C1"), ptr("1.1")
	idOpen := insertQuestion(t, db, open1)
	idPending1 := insertQuestion(t, db, catQuestion("txn-2"))
	idPending2 := insertQuestion(t, db, catQuestion("txn-3"))
	dry := catQuestion("txn-1")
	dry.Kind = QuestionDryRunCheck
	insertQuestion(t, db, dry)
	other := catQuestion("txn-9")
	other.PlanID = "plan-2"
	insertQuestion(t, db, other)

	if open, pending, err := db.CountQuestions(ctx, plan); err != nil || open != 1 || pending != 3 {
		t.Errorf("CountQuestions = %d open, %d pending, %v; want 1, 3", open, pending, err)
	}
	if got, _ := db.OpenQuestions(ctx, plan); len(got) != 1 || got[0].ID != idOpen {
		t.Errorf("OpenQuestions = %+v", got)
	}
	got, _ := db.PendingQuestions(ctx, plan)
	if len(got) != 3 || got[0].ID != idPending1 || got[1].ID != idPending2 {
		t.Errorf("PendingQuestions = %+v", got)
	}

	// One live question per plan, kind and subject.
	if _, err := db.InsertQuestion(ctx, catQuestion("txn-2")); err == nil {
		t.Error("second live categorize question for txn-2 was stored")
	}
	if closed, err := db.CloseQuestion(ctx, idPending1, "categorized in YNAB"); err != nil || !closed {
		t.Fatalf("CloseQuestion = %v %v", closed, err)
	}
	if closed, _ := db.CloseQuestion(ctx, idPending1, "again"); closed {
		t.Error("closed a closed question twice")
	}
	again := insertQuestion(t, db, catQuestion("txn-2"))
	bySubject, _ := db.QuestionsBySubject(ctx, plan, "txn-2")
	if len(bySubject) != 2 || bySubject[0].Status != QuestionClosed || *bySubject[0].CloseReason != "categorized in YNAB" ||
		bySubject[1].ID != again {
		t.Errorf("QuestionsBySubject = %+v", bySubject)
	}
	if open, pending, _ := db.CountQuestions(ctx, plan); open != 1 || pending != 3 {
		t.Errorf("after close and re-ask: %d open, %d pending; want 1, 3", open, pending)
	}
	if open, pending, _ := db.CountQuestions(ctx, "plan-none"); open != 0 || pending != 0 {
		t.Errorf("empty plan counts = %d, %d", open, pending)
	}
}

func TestAnsweredQuestionsInRange(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	answer := func(subject, kind, ts string, at time.Time) {
		q := catQuestion(subject)
		q.Kind, q.ChannelID, q.MessageTS = kind, ptr("C1"), ptr(ts)
		id := insertQuestion(t, db, q)
		if won, err := db.ClaimAnswer(ctx, id, "yes", UserBrendan, at); err != nil || !won {
			t.Fatalf("ClaimAnswer: %v %v", won, err)
		}
	}
	answer("t-before", QuestionDryRunCheck, "1", t0.Add(-time.Hour))
	answer("t-in2", QuestionDryRunCheck, "2", t0.Add(2*time.Hour))
	answer("t-in1", QuestionDryRunCheck, "3", t0)
	answer("t-cat", QuestionCategorize, "4", t0.Add(time.Hour))
	answer("t-end", QuestionDryRunCheck, "5", t0.Add(24*time.Hour))
	openQ := catQuestion("t-open")
	openQ.Kind, openQ.ChannelID, openQ.MessageTS = QuestionDryRunCheck, ptr("C1"), ptr("6")
	insertQuestion(t, db, openQ)

	got, err := db.AnsweredQuestions(ctx, plan, QuestionDryRunCheck, t0, t0.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SubjectID != "t-in1" || got[1].SubjectID != "t-in2" {
		t.Errorf("AnsweredQuestions = %+v", got)
	}
}

func TestInsertQuestionRejectsBadRows(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	for name, mutate := range map[string]func(*Question){
		"kind":       func(q *Question) { q.Kind = "chat" },
		"subject":    func(q *Question) { q.SubjectID = "" },
		"half-key":   func(q *Question) { q.ChannelID = ptr("C1") },
		"half-key-2": func(q *Question) { q.MessageTS = ptr("1.1") },
	} {
		q := catQuestion("txn-" + name)
		mutate(&q)
		if _, err := db.InsertQuestion(ctx, q); err == nil {
			t.Errorf("%s: bad question stored", name)
		}
	}
	empty := catQuestion("txn-empty")
	empty.Options = nil
	id := insertQuestion(t, db, empty)
	if got, _, _ := db.QuestionByID(ctx, id); got.Options == nil || len(got.Options) != 0 {
		t.Errorf("nil options read back as %#v, want empty", got.Options)
	}
}

func TestUpsertUsers(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	if err := db.UpsertUsers(ctx, []User{{"UB", UserBrendan}, {"UW", UserWife}}, t0); err != nil {
		t.Fatal(err)
	}
	if u, ok, _ := db.UserBySlackID(ctx, "UW"); !ok || u.Label != UserWife {
		t.Errorf("UW = %+v %v", u, ok)
	}
	if _, ok, _ := db.UserBySlackID(ctx, "UX"); ok {
		t.Error("unknown Slack id resolved")
	}
	// Config changes: Brendan's id changes, and the labels survive a swap.
	if err := db.UpsertUsers(ctx, []User{{"UB2", UserBrendan}}, t0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := db.UserBySlackID(ctx, "UB"); ok {
		t.Error("old Slack id still maps after its label moved")
	}
	if err := db.UpsertUsers(ctx, []User{{"UB2", UserWife}, {"UW", UserBrendan}}, t0); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := db.UserBySlackID(ctx, "UB2"); u.Label != UserWife {
		t.Errorf("UB2 = %+v, want wife", u)
	}
	if u, _, _ := db.UserBySlackID(ctx, "UW"); u.Label != UserBrendan {
		t.Errorf("UW = %+v, want brendan", u)
	}
	if n := count(t, db, `SELECT count(*) FROM users`); n != 2 {
		t.Errorf("users = %d rows, want 2", n)
	}
	if err := db.UpsertUsers(ctx, []User{{"UZ", "kid"}}, t0); err == nil {
		t.Error("unknown label stored")
	}
}

func TestSlackEventDedupeAndPrune(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	seen := func(key string, at time.Time) bool {
		t.Helper()
		dup, err := db.SeenSlackEvent(ctx, key, at)
		if err != nil {
			t.Fatal(err)
		}
		return dup
	}
	old := t0.Add(-8 * 24 * time.Hour)
	if seen("Ev-old", old) || seen("Ev-new", t0.Add(-time.Hour)) {
		t.Fatal("first sighting reported as duplicate")
	}
	if !seen("Ev-new", t0) {
		t.Error("repeated key not reported as duplicate")
	}
	n, err := db.PruneSlackEvents(ctx, t0)
	if err != nil || n != 1 {
		t.Fatalf("PruneSlackEvents = %d %v, want 1", n, err)
	}
	if !seen("Ev-new", t0) {
		t.Error("recent key pruned")
	}
	if seen("Ev-old", t0) {
		t.Error("old key survived the prune")
	}
}

func TestImportStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	if _, ok, err := db.GetImportState(ctx, plan); err != nil || ok {
		t.Fatalf("empty import state: ok=%v err=%v", ok, err)
	}
	if err := db.RecordImport(ctx, plan, t0, nil); err != nil {
		t.Fatal(err)
	}
	failAt := t0.Add(6 * time.Hour)
	if err := db.RecordImport(ctx, plan, failAt, ptr("ynab: 503 [redacted]")); err != nil {
		t.Fatal(err)
	}
	st, ok, err := db.GetImportState(ctx, plan)
	if err != nil || !ok || !st.LastAttemptAt.Equal(failAt) || st.LastOKAt == nil || !st.LastOKAt.Equal(t0) ||
		st.LastError == nil || *st.LastError != "ynab: 503 [redacted]" {
		t.Errorf("after failure = %+v %v %v", st, ok, err)
	}
	okAt := t0.Add(12 * time.Hour)
	if err := db.RecordImport(ctx, plan, okAt, nil); err != nil {
		t.Fatal(err)
	}
	st, _, _ = db.GetImportState(ctx, plan)
	if !st.LastAttemptAt.Equal(okAt) || !st.LastOKAt.Equal(okAt) || st.LastError != nil {
		t.Errorf("after success = %+v", st)
	}
	if err := db.RecordImport(ctx, "plan-2", t0, ptr("boom")); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := db.GetImportState(ctx, "plan-2"); st.LastOKAt != nil {
		t.Errorf("never-succeeded plan has last_ok_at %v", st.LastOKAt)
	}
}

func TestAlertCursorRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	if _, ok, err := db.AlertCursor(ctx, plan); err != nil || ok {
		t.Fatalf("empty cursor: ok=%v err=%v", ok, err)
	}
	for _, at := range []time.Time{t0, t0.Add(time.Nanosecond)} {
		if err := db.SetAlertCursor(ctx, plan, at); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := db.AlertCursor(ctx, plan); err != nil || !ok || !got.Equal(at) {
			t.Errorf("cursor = %v %v %v, want %v", got, ok, err, at)
		}
	}
}

func TestAlertsDedupeBudgetAndList(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	add := func(kind, key string, critical bool, status string, at time.Time) bool {
		t.Helper()
		_, inserted, err := db.InsertAlert(ctx, Alert{PlanID: plan, Kind: kind, DedupeKey: key, Critical: critical,
			Status: status, Body: kind + " " + key, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		return inserted
	}
	if !add("duplicate", "k1", false, AlertSent, t0) || !add("price", "k1", false, AlertRolledUp, t0.Add(time.Hour)) ||
		!add("link", "k2", true, AlertSent, t0.Add(2*time.Hour)) || !add("large", "k3", false, AlertSent, t0.AddDate(0, 1, 0)) {
		t.Fatal("first alert not inserted")
	}
	if add("duplicate", "k1", true, AlertSent, t0.Add(3*time.Hour)) {
		t.Error("repeated dedupe key inserted")
	}
	if n, err := db.CountSentNonCritical(ctx, plan, t0, t0.AddDate(0, 1, 0)); err != nil || n != 1 {
		t.Errorf("CountSentNonCritical = %d %v, want 1", n, err)
	}
	got, err := db.Alerts(ctx, plan, t0, t0.AddDate(0, 1, 0))
	if err != nil || len(got) != 3 || got[1].Status != AlertRolledUp || !got[2].Critical || got[0].Body != "duplicate k1" {
		t.Errorf("Alerts = %+v %v", got, err)
	}
	if _, _, err := db.InsertAlert(ctx, Alert{PlanID: plan, Kind: "x", DedupeKey: "y", Status: "queued"}); err == nil {
		t.Error("unknown status stored")
	}
}

func TestTransactionsFirstSeenAfter(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	seedMirror(t, db, plan, []Transaction{labeled("old", "groc")}, nil)
	newer := labeled("new", "groc")
	gone := labeled("gone", "groc")
	gone.Deleted = true
	later := testSyncedAt.Add(time.Hour)
	if err := db.Update(ctx, func(tx *Tx) error {
		return tx.UpsertTransactions(plan, []Transaction{newer, gone}, later)
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.TransactionsFirstSeenAfter(ctx, plan, testSyncedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "new" || !got[0].FirstSeenAt.Equal(later) || *got[0].PayeeName != "Shell" {
		t.Errorf("TransactionsFirstSeenAfter = %+v", got)
	}
	if got, _ := db.TransactionsFirstSeenAfter(ctx, plan, testSyncedAt.Add(-time.Second)); len(got) != 2 {
		t.Errorf("from before the first sync: %d rows, want 2", len(got))
	}
}

func TestPayeeCharges(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	txn := func(id, date string, amount int64, payee *string) Transaction {
		return Transaction{ID: id, Date: date, Amount: amount, Cleared: "cleared", Approved: true, AccountID: "a1",
			PayeeID: payee, CategoryID: ptr("groc")}
	}
	imported := txn("imp", "2026-09-20", -700, nil)
	imported.ImportPayeeName = ptr("COSTCO INC")
	imported.ImportID = ptr("YNAB:-700:2026-09-20:1")
	deleted := txn("del", "2026-09-10", -900, ptr("costco"))
	deleted.Deleted = true
	transfer := txn("xfer", "2026-09-11", -900, ptr("costco"))
	transfer.TransferAccountID = ptr("a1")
	seedMirror(t, db, plan, []Transaction{
		txn("c1", "2026-09-01", -5000, ptr("costco")),
		txn("c2", "2026-10-01", -6000, ptr("costco2")), // "COSTCO  INC" normalizes the same
		txn("c3", "2026-10-15", -7000, ptr("costco")),  // on the before date: excluded
		txn("refund", "2026-09-05", 2000, ptr("costco")),
		txn("shell", "2026-09-02", -3000, ptr("shell")),
		imported, deleted, transfer,
	}, nil)

	got, err := db.PayeeCharges(ctx, plan, NormalizePayee("Costco Inc"), "2026-10-15")
	if err != nil {
		t.Fatal(err)
	}
	want := []PayeeCharge{{"c1", "2026-09-01", -5000, nil}, {"imp", "2026-09-20", -700, ptr("YNAB:-700:2026-09-20:1")},
		{"c2", "2026-10-01", -6000, nil}}
	if len(got) != len(want) {
		t.Fatalf("PayeeCharges = %+v, want %+v", got, want)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.TxnID != w.TxnID || g.Date != w.Date || g.Amount != w.Amount || (g.ImportID == nil) != (w.ImportID == nil) ||
			(g.ImportID != nil && *g.ImportID != *w.ImportID) {
			t.Errorf("PayeeCharges[%d] = %+v, want %+v", i, g, w)
		}
	}
	if got, _ := db.PayeeCharges(ctx, plan, NormalizePayee("Costco Inc"), "2026-09-01"); len(got) != 0 {
		t.Errorf("before the first charge: %+v", got)
	}
}

func TestDryRunReportReads(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	seedMirror(t, db, plan, []Transaction{labeled("t1", "dine"), labeled("t2", "groc")}, nil)
	if err := db.Lock(ctx, HumanLock{PlanID: plan, TxnID: "t1", Field: LockFieldCategory, Source: LockYNABEdit, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}

	dec := func(txn string, at time.Time) Decision {
		return Decision{PlanID: plan, TxnID: txn, CategoryID: ptr("groc"), CategoryName: ptr("Groceries"),
			Confidence: ptr(0.9), Method: MethodModel, Model: "m", Mode: ModeDryRun, Outcome: OutcomeWouldApply,
			Features: json.RawMessage(`{}`), CreatedAt: at}
	}
	for _, d := range []Decision{dec("t1", t0.Add(-time.Hour)), dec("t1", t0), dec("t2", t0.Add(time.Hour)),
		dec("t2", t0.Add(48*time.Hour))} {
		if _, err := db.InsertDecision(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.DecisionsBetween(ctx, plan, t0, t0.Add(24*time.Hour))
	if err != nil || len(got) != 2 || got[0].TxnID != "t1" || got[1].TxnID != "t2" || !got[0].CreatedAt.Equal(t0) {
		t.Errorf("DecisionsBetween = %+v %v", got, err)
	}
	if got[0].SupersededBy != nil || got[1].SupersededBy == nil {
		t.Errorf("superseded_by: t1 %v, t2 %v", got[0].SupersededBy, got[1].SupersededBy)
	}

	for _, at := range []time.Time{t0.Add(-time.Minute), t0, t0.Add(time.Hour)} {
		e := writeIntent("t1")
		e.CreatedAt = at
		insertIntent(t, db, e)
	}
	if n, err := db.CountWriteLog(ctx, plan, t0, t0.Add(24*time.Hour)); err != nil || n != 2 {
		t.Errorf("CountWriteLog = %d %v, want 2", n, err)
	}
	if n, _ := db.CountWriteLog(ctx, "plan-2", t0, t0.Add(24*time.Hour)); n != 0 {
		t.Errorf("CountWriteLog other plan = %d", n)
	}

	// t1: approved and locked after its decision; the lock time is when the label was seen.
	h, ok, err := db.CurrentHumanCategory(ctx, plan, "t1")
	if err != nil || !ok || !h.HumanSet || *h.CategoryID != "dine" || *h.CategoryName != "Dining" || !h.Locked ||
		!h.LockedAt.Equal(t0) || !h.SeenAt.Equal(t0) || !h.FirstSeenAt.Equal(testSyncedAt) || h.IsSplit || h.Deleted {
		t.Errorf("CurrentHumanCategory t1 = %+v %v %v", h, ok, err)
	}
	// t2: approved, no lock: human-set, but seen only as of first_seen_at.
	if h, _, _ := db.CurrentHumanCategory(ctx, plan, "t2"); !h.HumanSet || h.Locked || h.LockedAt != nil ||
		*h.CategoryName != "Groceries" || !h.SeenAt.Equal(testSyncedAt) {
		t.Errorf("CurrentHumanCategory t2 = %+v", h)
	}
	if _, ok, err := db.CurrentHumanCategory(ctx, plan, "nope"); ok || err != nil {
		t.Errorf("unknown txn: ok=%v err=%v", ok, err)
	}
}

func TestCurrentHumanCategoryExcludesGuessesAndInternal(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	guess := labeled("guess", "groc") // YNAB payee memory on an unapproved import
	guess.Approved = false
	guessLocked := labeled("guess-locked", "groc")
	guessLocked.Approved = false
	unc := labeled("unc", "unc")
	rta := labeled("rta", "rta")
	none := labeled("none", "groc")
	none.CategoryID = nil
	split := labeled("split", "split-sentinel")
	seedMirror(t, db, plan, []Transaction{guess, guessLocked, unc, rta, none, split}, []Subtransaction{
		{ID: "s1", TransactionID: "split", Amount: -500, CategoryID: ptr("groc")},
		{ID: "s2", TransactionID: "split", Amount: -500, CategoryID: ptr("dine")},
	})
	if err := db.Lock(ctx, HumanLock{PlanID: plan, TxnID: "guess-locked", Field: LockFieldCategory, Source: LockYNABEdit,
		CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"guess", "unc", "rta", "none"} {
		h, ok, err := db.CurrentHumanCategory(ctx, plan, id)
		if err != nil || !ok || h.HumanSet || h.CategoryID != nil || h.CategoryName != nil {
			t.Errorf("%s = %+v %v %v, want not human-set and no category", id, h, ok, err)
		}
	}
	if h, _, _ := db.CurrentHumanCategory(ctx, plan, "guess-locked"); !h.HumanSet || *h.CategoryName != "Groceries" ||
		!h.SeenAt.Equal(t0) {
		t.Errorf("locked unapproved = %+v, want human-set as of the lock", h)
	}
	if h, _, _ := db.CurrentHumanCategory(ctx, plan, "split"); !h.HumanSet || !h.IsSplit || h.CategoryID != nil {
		t.Errorf("split = %+v, want human-set split with no single category", h)
	}
}

func TestHasAndDeleteAlert(t *testing.T) {
	ctx := context.Background()
	db, _ := openTest(t)
	a := Alert{PlanID: plan, Kind: "link_error", DedupeKey: "Checking", Critical: true, Status: AlertSent, Body: "b", CreatedAt: t0}
	if has, err := db.HasAlert(ctx, plan, a.Kind, a.DedupeKey); err != nil || has {
		t.Fatalf("HasAlert before insert = %v %v", has, err)
	}
	if _, inserted, err := db.InsertAlert(ctx, a); err != nil || !inserted {
		t.Fatal(err)
	}
	if has, _ := db.HasAlert(ctx, plan, a.Kind, a.DedupeKey); !has {
		t.Error("HasAlert after insert = false")
	}
	if has, _ := db.HasAlert(ctx, "plan-2", a.Kind, a.DedupeKey); has {
		t.Error("HasAlert matched another plan")
	}
	if removed, err := db.DeleteAlert(ctx, plan, a.Kind, a.DedupeKey); err != nil || !removed {
		t.Fatalf("DeleteAlert = %v %v", removed, err)
	}
	if removed, _ := db.DeleteAlert(ctx, plan, a.Kind, a.DedupeKey); removed {
		t.Error("second DeleteAlert removed a row")
	}
	// The episode cleared, so the same key alerts again.
	if _, inserted, err := db.InsertAlert(ctx, a); err != nil || !inserted {
		t.Errorf("re-insert after delete = %v %v", inserted, err)
	}
}
