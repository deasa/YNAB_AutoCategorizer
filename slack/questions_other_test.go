package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	catRepair  = "cat-repair-uuid"  // "Home - Repairs": makes "home" ambiguous
	catHidden  = "cat-hidden-uuid"  // hidden category
	catDeleted = "cat-deleted-uuid" // deleted category
	catHidGrp  = "cat-hidgrp-uuid"  // visible category in a hidden group
	catForeign = "cat-foreign-uuid" // a category of another plan
	catNet     = "cat-net-uuid"     // "Subs - Netflix"
	catRTA     = "cat-rta-uuid"     // "Inflow: Ready to Assign", internal
	catUncat   = "cat-uncat-uuid"   // "Uncategorized", internal
	catVisa    = "cat-visa-uuid"    // "Visa Card", in Credit Card Payments
	qGroupInt  = "group-7777-uuid"
	qGroupCCP  = "group-8888-uuid"
	qGroup2    = "group-5555-uuid"
	qGroupHid  = "group-6666-uuid"
)

// addCats adds a second visible group, a hidden group and another plan's
// category to the seeded mirror.
func addCats(t *testing.T, db *store.DB) {
	t.Helper()
	err := db.Update(context.Background(), func(tx *store.Tx) error {
		if err := tx.UpsertCategoryGroups(qPlan, []store.CategoryGroup{{ID: qGroup2, Name: "Bills"},
			{ID: qGroupHid, Name: "Old Stuff", Hidden: true},
			{ID: qGroupInt, Name: "Internal Master Category", Internal: true},
			{ID: qGroupCCP, Name: "Credit Card Payments"}}); err != nil {
			return err
		}
		if err := tx.UpsertCategories(qPlan, []store.Category{
			{ID: catRepair, CategoryGroupID: qGroup2, Name: "Home - Repairs"},
			{ID: catNet, CategoryGroupID: qGroup2, Name: "Subs - Netflix"},
			{ID: catHidden, CategoryGroupID: qGroup, Name: "Secret Hidden", Hidden: true},
			{ID: catDeleted, CategoryGroupID: qGroup, Name: "Gone Deleted", Deleted: true},
			{ID: catHidGrp, CategoryGroupID: qGroupHid, Name: "Buried Category"},
			{ID: catRTA, CategoryGroupID: qGroupInt, Name: "Inflow: Ready to Assign", Internal: true},
			{ID: catUncat, CategoryGroupID: qGroupInt, Name: "Uncategorized", Internal: true},
			{ID: catVisa, CategoryGroupID: qGroupCCP, Name: "Visa Card"},
		}); err != nil {
			return err
		}
		if err := tx.UpsertCategoryGroups("plan-9999-uuid", []store.CategoryGroup{{ID: "g-9", Name: "Elsewhere"}}); err != nil {
			return err
		}
		return tx.UpsertCategories("plan-9999-uuid", []store.Category{{ID: catForeign, CategoryGroupID: "g-9", Name: "Foreign Cat"}})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// askWith inserts and posts a question with the given options.
func (e *qenv) askWith(t *testing.T, opts []store.QuestionOption) int64 {
	t.Helper()
	id, err := e.db.InsertQuestion(context.Background(), store.Question{PlanID: qPlan, Kind: store.QuestionCategorize,
		SubjectID: qTxn, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.q.Ask(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// selectOptions returns the select's option texts and values, in order.
func selectOptions(t *testing.T, blocks string) (texts, vals []string) {
	t.Helper()
	var bs []struct {
		Elements []struct {
			Type         string `json:"type"`
			OptionGroups []struct {
				Options []struct {
					Text  struct{ Text string } `json:"text"`
					Value string                `json:"value"`
				} `json:"options"`
			} `json:"option_groups"`
		} `json:"elements"`
	}
	if err := json.Unmarshal([]byte(blocks), &bs); err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		for _, el := range b.Elements {
			for _, g := range el.OptionGroups {
				for _, o := range g.Options {
					texts = append(texts, o.Text.Text)
					vals = append(vals, o.Value)
				}
			}
		}
	}
	return texts, vals
}

func TestOneOptionStillOffersEveryVisibleCategory(t *testing.T) {
	db, path := newStore(t)
	seedMirror(t, db, "Costco", "weekly shop")
	addCats(t, db)
	var logs bytes.Buffer
	e := &qenv{db: db, path: path, msg: &recMessenger{t: t}, apply: &fakeApplier{}}
	e.q = NewQuestions(db, e.apply, e.msg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	id := e.askWith(t, []store.QuestionOption{{Label: "Gas", CategoryID: catGas, CategoryName: "Gas"}})
	qid := strconv.FormatInt(id, 10)

	post := e.msg.posts[0].blocks
	if !strings.Contains(post, "other…") || !strings.Contains(post, actionOther) {
		t.Fatalf("one-option question has no other…: %s", post)
	}
	if !strings.Contains(post, hintText) {
		t.Fatalf("question lacks the hint: %s", post)
	}

	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionOther, qid))
	u := e.msg.lastUpdate(t)
	texts, vals := selectOptions(t, u.blocks)
	want := []string{"Gas", "Home - Repairs", "Subs - Netflix", "Dining Out", "Fun Money", "Gifts", "Groceries", "Home & Garden"}
	if fmt.Sprint(texts) != fmt.Sprint(want) {
		t.Fatalf("select = %v, want %v (option first, then by group)", texts, want)
	}
	if !strings.Contains(u.blocks, hintText) || !strings.Contains(u.blocks, `"Suggested"`) || !strings.Contains(u.blocks, `"Bills"`) {
		t.Fatalf("update = %s", u.blocks)
	}
	for _, yid := range append(allIDs, catRepair, catNet, catHidden, catDeleted, catHidGrp, catForeign, catRTA, catUncat, catVisa) {
		if strings.Contains(u.blocks, yid) {
			t.Fatalf("select carries YNAB id %q", yid)
		}
	}

	// Forged values: hidden, deleted, hidden-group, foreign, junk, and a raw id.
	for _, v := range []string{catHidden, catDeleted, catHidGrp, catForeign, catRTA, catUncat, catVisa} {
		for _, val := range []string{qid + ":c:" + catKey(v), qid + ":c:" + v} {
			e.q.HandleClick(context.Background(), click(brendan, "501.1", actionSelect, val))
		}
	}
	for _, val := range []string{qid + ":c:", qid + ":c:zz", "999:c:" + catKey(catGroc)} {
		e.q.HandleClick(context.Background(), click(brendan, "501.1", actionSelect, val))
	}
	// A pick button never takes a category key.
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionPick+"0", qid+":c:"+catKey(catGroc)))
	if n := e.apply.calls.Load(); n != 0 {
		t.Fatalf("a forged value applied (%d calls)", n)
	}
	if q := e.question(t, id); q.Status != store.QuestionOpen {
		t.Fatalf("status = %s", q.Status)
	}

	// A real select value for a plan category answers it.
	i := -1
	for j, s := range texts {
		if s == "Home - Repairs" {
			i = j
		}
	}
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionSelect, vals[i]))
	if e.apply.calls.Load() != 1 || e.apply.got[0].Writes[0].CategoryID != catRepair {
		t.Fatalf("apply = %+v", e.apply.got)
	}
	if q := e.question(t, id); q.Status != store.QuestionAnswered || *q.Answer != catRepair {
		t.Fatalf("question = %+v", q)
	}
	for _, yid := range append(allIDs, catRepair, catKey(catRepair)) {
		if strings.Contains(logs.String(), yid) {
			t.Fatalf("log carries id %q: %s", yid, logs.String())
		}
	}
}

func TestSelectCapsAtBlockKitLimit(t *testing.T) {
	var cats []store.CategoryWithGroup
	for i := range 150 {
		cats = append(cats, store.CategoryWithGroup{ID: fmt.Sprintf("c%03d", i), Name: fmt.Sprintf("Cat %03d", i),
			GroupID: fmt.Sprintf("g%d", i/20), GroupName: fmt.Sprintf("Group %d", i/20)})
	}
	qu := store.Question{ID: 7, Options: []store.QuestionOption{{Label: "Cat 149", CategoryID: "c149", CategoryName: "Cat 149"}}}
	groups, dropped := selectGroups(qu, cats)
	n := 0
	for _, g := range groups {
		n += len(g.Options)
	}
	if n != maxSelect || dropped != 50 || groups[0].Options[0].Text.Text != "Cat 149" {
		t.Fatalf("options = %d, dropped = %d, first = %q", n, dropped, groups[0].Options[0].Text.Text)
	}
}

func TestLooseRepliesResolve(t *testing.T) {
	for _, tc := range []struct{ text, cat string }{
		{"groceries", catGroc},
		{"Groceries!", catGroc},
		{"dining out", catDine},
		{"DINING-OUT.", catDine},
		{"home and garden", catHome},
		{"netflix", catNet},                       // "Subs - " prefix ignored
		{"this was groceries at costco", catGroc}, // one name as whole words
		{"groc", catGroc},                         // unique prefix
		{"repairs", catRepair},
	} {
		db, path := newStore(t)
		seedMirror(t, db, "Costco", "weekly shop")
		addCats(t, db)
		var logs bytes.Buffer
		e := &qenv{db: db, path: path, msg: &recMessenger{t: t}, apply: &fakeApplier{}}
		e.q = NewQuestions(db, e.apply, e.msg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		id := e.ask(t, store.QuestionCategorize, qTxn)
		e.q.HandleReply(context.Background(), Message{User: brendan, Channel: testChannelID, TS: "600.1",
			ThreadTS: "501.1", Text: tc.text})
		if e.apply.calls.Load() != 1 || e.apply.got[0].Writes[0].CategoryID != tc.cat {
			t.Fatalf("%q: apply = %+v", tc.text, e.apply.got)
		}
		if q := e.question(t, id); q.Status != store.QuestionAnswered || *q.Answer != tc.cat {
			t.Fatalf("%q: question = %+v", tc.text, q)
		}
		if strings.Contains(logs.String(), tc.text) {
			t.Fatalf("%q: log carries the reply: %s", tc.text, logs.String())
		}
	}
}

func TestAmbiguousReplySuggestsAndStaysOpen(t *testing.T) {
	for _, text := range []string{"home", "zzzz nothing"} {
		db, path := newStore(t)
		seedMirror(t, db, "Costco", "weekly shop")
		addCats(t, db)
		var logs bytes.Buffer
		e := &qenv{db: db, path: path, msg: &recMessenger{t: t}, apply: &fakeApplier{}}
		e.q = NewQuestions(db, e.apply, e.msg, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		id := e.ask(t, store.QuestionDryRunCheck, qTxn)
		e.q.HandleReply(context.Background(), Message{User: brendan, Channel: testChannelID, TS: "600.1",
			ThreadTS: "501.1", Text: text})
		if q := e.question(t, id); q.Status != store.QuestionOpen {
			t.Fatalf("%q: status = %s", text, q.Status)
		}
		if len(e.msg.posts) != 2 || e.msg.posts[1].ts != "501.1" {
			t.Fatalf("%q: posts = %+v", text, e.msg.posts)
		}
		reply := e.msg.posts[1].text
		if !strings.Contains(reply, "Reply with one of these, or use other…") || strings.Count(reply, `"`) > 2*maxSuggestion {
			t.Fatalf("%q: reply = %q", text, reply)
		}
		if text == "home" && (!strings.Contains(reply, `"Home & Garden"`) || !strings.Contains(reply, `"Home - Repairs"`)) {
			t.Fatalf("reply = %q", reply)
		}
		for _, hidden := range []string{"Secret Hidden", "Gone Deleted", "Buried Category", "Foreign Cat"} {
			if strings.Contains(reply, hidden) {
				t.Fatalf("%q: suggested %q", text, hidden)
			}
		}
		if strings.Contains(logs.String(), text) || strings.Contains(logs.String(), "-uuid") {
			t.Fatalf("%q: log = %s", text, logs.String())
		}
	}
}

func TestNormFolds(t *testing.T) {
	for in, want := range map[string]string{
		"Home & Garden":   "home garden",
		"home and garden": "home garden",
		"  Groceries!! ":  "groceries",
		"🍔 Dining-Out":    "dining out",
		"Subs - Netflix":  "subs netflix",
		"":                "",
	} {
		if got := norm(in); got != want {
			t.Errorf("norm(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExcludedCategoriesNeverAnswer(t *testing.T) {
	excluded := []string{"Inflow: Ready to Assign", "Uncategorized", "Visa Card", "visa", "ready to assign",
		"Secret Hidden", "Buried Category"}
	for _, text := range excluded {
		db, path := newStore(t)
		seedMirror(t, db, "Costco", "weekly shop")
		addCats(t, db)
		e := &qenv{db: db, path: path, msg: &recMessenger{t: t}, apply: &fakeApplier{}}
		e.q = NewQuestions(db, e.apply, e.msg, nil)
		id := e.ask(t, store.QuestionCategorize, qTxn)
		e.q.HandleReply(context.Background(), Message{User: brendan, Channel: testChannelID, TS: "600.1",
			ThreadTS: "501.1", Text: text})
		if e.apply.calls.Load() != 0 {
			t.Fatalf("%q: applied %+v", text, e.apply.got)
		}
		if q := e.question(t, id); q.Status != store.QuestionOpen {
			t.Fatalf("%q: status = %s", text, q.Status)
		}
		if len(e.msg.posts) != 2 || !strings.Contains(e.msg.posts[1].text, "Reply with one of these, or use other…") {
			t.Fatalf("%q: posts = %+v", text, e.msg.posts)
		}
		for _, name := range []string{"Ready to Assign", "Uncategorized", "Visa Card", "Secret Hidden", "Buried Category"} {
			if strings.Contains(e.msg.posts[1].text, name) {
				t.Fatalf("%q: suggested %q: %q", text, name, e.msg.posts[1].text)
			}
		}
	}
}

func TestSelectKeepsSetAsides(t *testing.T) {
	db, path := newStore(t)
	seedMirror(t, db, "Costco", "weekly shop")
	addCats(t, db)
	e := &qenv{db: db, path: path, msg: &recMessenger{t: t}, apply: &fakeApplier{}}
	e.q = NewQuestions(db, e.apply, e.msg, nil)
	id := e.ask(t, store.QuestionCategorize, qTxn)
	e.q.HandleClick(context.Background(), click(brendan, "501.1", actionOther, strconv.FormatInt(id, 10)))
	texts, _ := selectOptions(t, e.msg.lastUpdate(t).blocks)
	if !strings.Contains(fmt.Sprint(texts), "Gifts") { // a set-aside (policy keyword "Gift")
		t.Fatalf("select = %v", texts)
	}
	e.q.HandleReply(context.Background(), Message{User: brendan, Channel: testChannelID, TS: "600.1",
		ThreadTS: "501.1", Text: "gifts"})
	if e.apply.calls.Load() != 1 || e.apply.got[0].Writes[0].CategoryID != catOther {
		t.Fatalf("apply = %+v", e.apply.got)
	}
}
