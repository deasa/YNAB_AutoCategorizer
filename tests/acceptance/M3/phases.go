package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/calibrate"
	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

func (h *harness) mirrorCat(txnID string) string {
	r, ok, err := h.db.TransactionByID(h.ctx, h.sb, txnID)
	if err != nil || !ok || r.CategoryID == nil {
		return ""
	}
	return *r.CategoryID
}

func jsonEq(raw json.RawMessage, want map[string]any) bool {
	var got map[string]any
	return json.Unmarshal(raw, &got) == nil && reflect.DeepEqual(got, want)
}

// Scope 2: the writer applies and logs before/after, refuses a row a human
// edited after the decision, and a human edit made after finbot's write
// survives later syncs and later writes.
func (h *harness) phaseWriter() {
	h.fixtures()
	h.openDB(daysAgo(30))
	h.mustSync("initial")
	h.pickCategories()
	catA, catB := h.cats[0], h.cats[1]
	w1, w2, w3 := h.createTxn("w apply", 0), h.createTxn("w pre-edit", 0), h.createTxn("w post-edit", 0)
	h.mustSync("fixtures")
	h.humanEdit(w2, catB) // a person categorizes it in YNAB after the decision was made
	h.gate.Set(writer.ModeAuto)

	p0 := h.g.patchCount()
	out, err := h.w.Apply(h.ctx, writer.Batch{PlanID: h.sb, Writes: []writer.Write{
		{TxnID: w1, CategoryID: catA, Approve: true},
		{TxnID: w2, CategoryID: catA, Approve: true},
		{TxnID: w3, CategoryID: catA, Approve: true},
	}}, store.ActorSystem, store.ViaScheduler)
	check(err == nil && len(out) == 3, "writer.Apply on 3 sandbox rows returns 3 outcomes (err=%v)", err)
	if len(out) != 3 {
		return
	}
	check(out[0].Status == writer.StatusDone && out[0].WriteID != 0, "row 1 applied: %s", out[0].Status)
	check(out[1].Status == writer.StatusSkipped && out[1].Reason == writer.ReasonCategoryChanged && out[1].WriteID == 0,
		"human-edited row refused: %s %q", out[1].Status, out[1].Reason)
	check(out[2].Status == writer.StatusDone, "row 3 applied: %s", out[2].Status)
	check(h.g.patchCount()-p0 == 1, "one bulk PATCH sent (saw %d)", h.g.patchCount()-p0)

	e, ok, err := h.db.LastDoneWrite(h.ctx, h.sb, w1)
	check(ok && err == nil, "write_log has a done row for row 1")
	if ok {
		check(jsonEq(e.BeforeJSON, map[string]any{"category_id": nil, "approved": false}), "before_json = {category_id: null, approved: false}")
		check(jsonEq(e.AfterJSON, map[string]any{"category_id": catA, "approved": true}), "after_json = {category_id: <applied>, approved: true}")
		check(e.Actor == store.ActorSystem && e.Via == store.ViaScheduler && e.RequestID != nil && e.Endpoint == writer.Endpoint,
			"write_log actor=%s via=%s endpoint=%s request id set=%t", e.Actor, e.Via, e.Endpoint, e.RequestID != nil)
	}
	n, _ := h.writeRows(w2)
	check(n == 0, "no write_log row for the human-edited row (%d)", n)
	c, ap := h.ynabCat(w1)
	check(catOK(c, catA) && ap, "YNAB: row 1 has the applied category, approved")
	c, _ = h.ynabCat(w2)
	check(catOK(c, catB), "YNAB: human-edited row keeps the person's category")

	// A human edit made after finbot's write.
	h.humanEdit(w3, catB)
	h.mustSync("after human edit")
	ls := h.locks(w3)
	check(strings.Contains(names(ls), "category/ynab_edit"), "sync locked the human-edited field: [%s]", names(ls))
	p1 := h.g.patchCount()
	out, err = h.w.Apply(h.ctx, writer.Batch{PlanID: h.sb, Writes: []writer.Write{{TxnID: w3, CategoryID: catA, Approve: true, DecidedCategoryID: &catB}}},
		store.ActorSystem, store.ViaScheduler)
	check(err == nil && len(out) == 1 && out[0].Status == writer.StatusSkipped && out[0].Reason == writer.ReasonLocked,
		"a later write to the human-edited row is refused as locked (%v)", out)
	check(h.g.patchCount() == p1, "no PATCH for the locked row")
	h.mustSync("later 1")
	h.mustSync("later 2")
	c, _ = h.ynabCat(w3)
	check(catOK(c, catB), "YNAB: human edit still in place after two more syncs")
	check(h.mirrorCat(w3) == catB, "mirror: human edit still in place after two more syncs")
	check(strings.Contains(names(h.locks(w3)), "category/ynab_edit"), "lock still held after later syncs")
	n, st := h.writeRows(w3)
	check(n == 1 && st[0] == "done", "write_log for the edited row: only finbot's original write (%v)", st)
}

// Scope 3: Slack routing, with the real Slack client and Questions over a fake socket.
func (h *harness) phaseSlack() {
	h.fixtures()
	h.openDB(daysAgo(30))
	h.mustSync("start")
	h.pickCategories()
	h.startSlack()
	s1, s2, s3 := h.createTxn("lo s1", 0), h.createTxn("lo s2", 0), h.createTxn("lo s3", 0)
	h.mustSync("fixtures")
	res := h.runPipeline(writer.ModeAuto, daysAgo(30), 45, h.calibration(true))
	check(res.Mode == writer.ModeAuto, "pipeline with a matching calibration runs in auto (%s)", res.Mode)
	q1, ok1 := h.question(s1, store.QuestionCategorize)
	q2, ok2 := h.question(s2, store.QuestionCategorize)
	q3, ok3 := h.question(s3, store.QuestionCategorize)
	check(ok1 && ok2 && ok3 && q1.Status == store.QuestionOpen && q2.Status == store.QuestionOpen && q3.Status == store.QuestionOpen &&
		q1.MessageTS != nil, "below-τ rows became open, posted categorize questions")
	if !(ok1 && ok2 && ok3) || q1.MessageTS == nil || q2.MessageTS == nil {
		return
	}

	// An unlisted user's click.
	p0 := h.g.patchCount()
	h.send(click(strangerSlack, q1, 0))
	time.Sleep(3 * time.Second)
	check(h.reload(q1).Status == store.QuestionOpen, "unlisted user's click: question still open")
	n, _ := h.writeRows(s1)
	c, _ := h.ynabCat(s1)
	check(n == 0 && c == "" && h.g.patchCount() == p0, "unlisted user's click: no write_log, no PATCH, YNAB uncategorized")

	// A double click (Slack redelivery of one action, a second click, and a race with the other user).
	ats := "1800000600.000001"
	h.send(clickAt(brendanSlack, q1, 0, ats), clickAt(brendanSlack, q1, 0, ats), click(brendanSlack, q1, 0), click(wifeSlack, q1, 1))
	settle(func() bool { n, st := h.writeRows(s1); return n > 0 && st[n-1] == "done" })
	a := h.reload(q1)
	n, st := h.writeRows(s1)
	check(a.Status == store.QuestionAnswered && a.Answer != nil, "double click: question answered once")
	check(n == 1 && st[0] == "done", "double click: exactly one write_log row, done (%v)", st)
	check(h.g.patchCount()-p0 == 1, "double click: exactly one PATCH (%d)", h.g.patchCount()-p0)
	c, ap := h.ynabCat(s1)
	check(a.Answer != nil && catOK(c, *a.Answer) && ap, "double click: YNAB has the answered category, approved")
	check(strings.Contains(names(h.locks(s1)), "category/slack"), "Slack answer stored a category lock [%s]", names(h.locks(s1)))

	// Restart: new process state (DB reopened, new client/Questions/writer), old messages still route.
	h.restart(daysAgo(30), writer.ModeDryRun)
	check(h.gate.Mode() == writer.ModeDryRun, "after restart the gate starts at dry_run")
	res = h.runPipeline(writer.ModeAuto, daysAgo(30), 45, h.calibration(true))
	check(res.Mode == writer.ModeAuto && res.Asked == 0, "first pipeline run after restart: auto, asks nothing new (asked=%d)", res.Asked)
	p1 := h.g.patchCount()
	h.send(click(wifeSlack, q2, 2))
	settle(func() bool { n, st := h.writeRows(s2); return n > 0 && st[n-1] == "done" })
	a2 := h.reload(q2)
	n, st = h.writeRows(s2)
	check(a2.Status == store.QuestionAnswered && a2.AnsweredBy != nil && *a2.AnsweredBy == store.UserWife,
		"after restart: click on a pre-restart message routed to its question")
	check(n == 1 && st[0] == "done" && h.g.patchCount()-p1 == 1, "after restart: the answer was written once (%v)", st)
	c, _ = h.ynabCat(s2)
	check(a2.Answer != nil && catOK(c, *a2.Answer), "after restart: YNAB has the answered category")
	check(strings.Contains(h.api.lastText(), "Categorized as"), "after restart: the message was edited to say it was categorized")
	check(h.reload(q3).Status == store.QuestionOpen, "third question left open for the mismatch phase")
	h.stop()
	<-h.runDone
}

// Scope 4: configured auto with a calibration whose candidates hash doesn't
// match runs as dry_run; a Slack answer to a categorize question then writes
// nothing. The categorize question is on a row older than the mismatched run's
// backlog window, so that run doesn't re-decide it (a re-decided row's question
// is superseded, which is also checked).
func (h *harness) phaseMismatch() {
	h.fixtures()
	h.openDB(daysAgo(60))
	h.mustSync("start")
	h.pickCategories()
	h.startSlack()
	m1, m2 := h.createTxn("lo m1 aged", 40), h.createTxn("lo m2", 0)
	h.mustSync("fixtures 1")
	res := h.runPipeline(writer.ModeAuto, daysAgo(60), 45, h.calibration(true))
	q1, ok1 := h.question(m1, store.QuestionCategorize)
	q2, ok2 := h.question(m2, store.QuestionCategorize)
	check(res.Mode == writer.ModeAuto && ok1 && ok2 && q1.Status == store.QuestionOpen && q1.MessageTS != nil,
		"setup: matching calibration ran auto and asked categorize questions (mode %s)", res.Mode)
	if !ok1 || !ok2 {
		return
	}

	c1 := h.createTxn("hi c1", 0)
	h.mustSync("fixtures 2")
	before := h.totalWrites()
	p0 := h.g.patchCount()
	cal := h.calibration(true)
	cal.CandidatesHash = calibrate.Current().CandidatesHash // measured on the live plan
	res = h.runPipeline(writer.ModeAuto, daysAgo(60), 30, cal)
	check(res.Mode == writer.ModeDryRun, "configured auto + mismatched candidates hash: effective mode %s", res.Mode)
	check(strings.Contains(strings.Join(res.Warnings, ";"), "category list changed since calibration"), "warning: %v", res.Warnings)
	check(h.gate.Mode() == writer.ModeDryRun, "writer gate published as %s", h.gate.Mode())
	_, okDry := h.question(c1, store.QuestionDryRunCheck)
	_, isCat := h.question(c1, store.QuestionCategorize)
	check(okDry && !isCat, "a 0.97 (≥ τ) row became a dry_run_check question, not a write or categorize question")
	n, _ := h.writeRows(c1)
	c, _ := h.ynabCat(c1)
	check(n == 0 && c == "", "≥ τ row: no write_log, YNAB uncategorized")
	r2 := h.reload(q2)
	check(r2.Status == store.QuestionClosed && r2.CloseReason != nil && *r2.CloseReason == classify.ReasonSuperseded,
		"a re-decided row's categorize question was superseded")
	check(h.reload(q1).Status == store.QuestionOpen, "the aged row's categorize question is still open")

	h.send(click(brendanSlack, q1, 0))
	settle(func() bool { return h.reload(q1).Status == store.QuestionAnswered })
	a := h.reload(q1)
	check(a.Status == store.QuestionAnswered, "categorize answer recorded")
	n, _ = h.writeRows(m1)
	c, _ = h.ynabCat(m1)
	check(n == 0 && c == "", "categorize answer under the mismatch: no write_log, YNAB uncategorized")
	check(h.totalWrites() == before && h.g.patchCount() == p0, "no write_log rows and no PATCH since the mismatched run")
	check(strings.Contains(h.api.lastText(), "(dry run)"), "Slack reply says it was recorded (dry run)")
	h.stop()
	<-h.runDone
}

// Scope 5: dry_run -> auto transition.
func (h *harness) phaseTransition() {
	h.fixtures()
	h.openDB(today())
	h.mustSync("start")
	h.pickCategories()
	h.startSlack()
	t0, t1, t3 := h.createTxn("hi t0 old", 10), h.createTxn("hi t1", 0), h.createTxn("lo t3", 0)
	h.mustSync("fixtures")
	res := h.runPipeline(writer.ModeDryRun, today(), 45, h.calibration(true))
	check(res.Mode == writer.ModeDryRun, "first run in dry_run")
	var dq [3]store.Question
	for i, id := range []string{t0, t1, t3} {
		q, ok := h.question(id, store.QuestionDryRunCheck)
		check(ok && q.Status == store.QuestionOpen && q.MessageTS != nil, "dry_run: row %d has an open, posted dry_run_check (found=%t status=%s posted=%t)", i, ok, q.Status, q.MessageTS != nil)
		dq[i] = q
	}
	if dq[0].ID == 0 || dq[2].ID == 0 {
		return
	}
	res = h.runPipeline(writer.ModeAuto, today(), 45, h.calibration(true))
	check(res.Mode == writer.ModeAuto && res.SupersededByAuto >= 3, "entering auto: mode %s, %d dry_run_checks superseded", res.Mode, res.SupersededByAuto)
	for i := range dq {
		q := h.reload(dq[i])
		check(q.Status == store.QuestionClosed && q.CloseReason != nil && *q.CloseReason == classify.ReasonSupersededByAuto,
			"row %d's dry_run_check closed as superseded_by_auto", i)
	}
	n, st := h.writeRows(t1)
	check(n == 1 && st[0] == "done", "control: the ≥ τ row dated today was written in auto (%v)", st)
	n, _ = h.writeRows(t0)
	c, _ := h.ynabCat(t0)
	check(n == 0 && c == "" && res.BeforeAutoSince >= 1, "row dated before CLASSIFY_AUTO_SINCE: not written (before_auto_since=%d)", res.BeforeAutoSince)
	_, ok := h.question(t3, store.QuestionCategorize)
	check(ok, "the below-τ row got a categorize question in auto")

	// Clicks on the closed dry_run_check messages, with the gate in auto.
	p0 := h.g.patchCount()
	h.send(click(brendanSlack, dq[2], 0), click(brendanSlack, dq[0], 0))
	time.Sleep(4 * time.Second)
	for i, idx := range []int{2, 0} {
		q := h.reload(dq[idx])
		check(q.Status == store.QuestionClosed && q.Answer == nil, "click %d on a closed dry_run_check: still closed, no answer", i)
	}
	n3, _ := h.writeRows(t3)
	n0, _ := h.writeRows(t0)
	c3, _ := h.ynabCat(t3)
	c0, _ := h.ynabCat(t0)
	check(n3 == 0 && n0 == 0 && c3 == "" && c0 == "" && h.g.patchCount() == p0, "clicked dry_run_checks: nothing written, no PATCH")

	// A Slack answer naming the pre-AutoSince row, straight at the writer.
	_, err := h.w.Apply(h.ctx, writer.Batch{PlanID: h.sb, Writes: []writer.Write{{TxnID: t0, CategoryID: h.cats[0], Approve: true}}},
		store.UserBrendan, store.ViaSlack)
	n0, _ = h.writeRows(t0)
	check(errors.Is(err, writer.ErrBeforeAutoSince) && n0 == 0 && h.g.patchCount() == p0, "writer refuses the pre-AutoSince row even from Slack (%v)", err)
	h.stop()
	<-h.runDone
}

// crashAPI lets the real PATCH reach YNAB, then dies before the writer can mark it done.
type crashAPI struct{ real writer.API }

func (c crashAPI) PatchTransactions(ctx context.Context, ch []ynab.TransactionChange) ([]ynab.PatchedTransaction, error) {
	out, err := c.real.PatchTransactions(ctx, ch)
	if err != nil {
		fmt.Println("FAIL crash1: the PATCH itself failed:", scrub(err))
		os.Exit(1)
	}
	fmt.Printf("INFO crash1: PATCH returned %d rows; exiting before done\n", len(out))
	os.Exit(7)
	return out, nil
}

type crashState struct{ Txn, Cat string }

func (h *harness) phaseCrash1() {
	h.fixtures()
	h.apiWrap = crashAPI{h.client}
	h.openDB(daysAgo(30))
	h.mustSync("start")
	h.pickCategories()
	x := h.createTxn("crash x1", 0)
	h.mustSync("fixture")
	b, _ := json.Marshal(crashState{x, h.cats[0]})
	if err := os.WriteFile(filepath.Join(h.work, "crash.json"), b, 0o600); err != nil {
		fatal("state", err)
	}
	h.gate.Set(writer.ModeAuto)
	_, err := h.w.Apply(h.ctx, writer.Batch{PlanID: h.sb, Writes: []writer.Write{{TxnID: x, CategoryID: h.cats[0], Approve: true}}},
		store.ActorSystem, store.ViaScheduler)
	check(false, "crash1: Apply returned without the simulated crash (%v)", err)
}

func (h *harness) phaseCrash2() {
	var cs crashState
	b, err := os.ReadFile(filepath.Join(h.work, "crash.json"))
	if err != nil || json.Unmarshal(b, &cs) != nil {
		fatal("state", fmt.Errorf("read crash.json: %v", err))
	}
	h.fixtures()
	h.openDB(daysAgo(30))
	h.pickCategories()
	n, st := h.writeRows(cs.Txn)
	check(n == 1 && st[0] == "done", "after serve restart + sync: the intent is done (%v)", st)
	ls := h.locks(cs.Txn)
	check(len(ls) == 0, "the reconciled row is not locked [%s]", names(ls))
	c, ap := h.ynabCat(cs.Txn)
	check(catOK(c, cs.Cat) && ap, "YNAB has finbot's category, approved")
	h.mustSync("extra 1")
	h.mustSync("extra 2")
	ls = h.locks(cs.Txn)
	check(len(ls) == 0, "still not locked after two more syncs [%s]", names(ls))
	open, err := h.db.UnfinishedWrites(h.ctx)
	check(err == nil && len(open) == 0, "no unfinished intents remain (%d)", len(open))
}
