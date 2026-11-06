package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const tagPlan = "plan-tag-test"

func tagEnv(t *testing.T) *store.DB {
	t.Helper()
	db := revokeEnv(t)
	t.Setenv("YNAB_BUDGET_ID", tagPlan)
	err := db.Update(context.Background(), func(tx *store.Tx) error {
		return tx.UpsertAccounts(tagPlan, []store.Account{
			{ID: "acct-401k-id", Name: "Fidelity 401k"},
			{ID: "acct-roth-id", Name: "Vanguard Roth", Closed: true},
			{ID: "acct-dup1-id", Name: "Savings", OnBudget: true},
			{ID: "acct-dup2-id", Name: "savings", OnBudget: true},
			{ID: "acct-gone-id", Name: "Old HSA", Deleted: true},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func runTag(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Main(append([]string{"tag-account"}, args...), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestTagAccount_SetsAndLists(t *testing.T) {
	db := tagEnv(t)
	if code, out, errOut := runTag("FIDELITY 401K", "Traditional"); code != 0 || out != "tagged Fidelity 401k as traditional\n" {
		t.Fatalf("tag = %d %q %q", code, out, errOut)
	}
	if code, _, errOut := runTag("Vanguard Roth", "roth"); code != 0 {
		t.Fatalf("tag closed account = %d %q", code, errOut)
	}
	// Retagging replaces the tag.
	if code, _, errOut := runTag("Fidelity 401k", "none"); code != 0 {
		t.Fatalf("retag = %d %q", code, errOut)
	}
	for _, args := range [][]string{nil, {"--list"}} {
		code, out, errOut := runTag(args...)
		if code != 0 {
			t.Fatalf("list %v = %d %q", args, code, errOut)
		}
		for _, want := range []string{"none         Fidelity 401k\n", "roth         Vanguard Roth (closed)\n", "untagged     Savings\n", "untagged     savings\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("list %v lacks %q:\n%s", args, want, out)
			}
		}
		if strings.Contains(out, "Old HSA") || strings.Contains(out, "acct-") || strings.Contains(out, tagPlan) {
			t.Errorf("list shows a deleted account or an id:\n%s", out)
		}
	}
	accts, err := db.Accounts(context.Background(), tagPlan)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accts {
		if a.Name == "Fidelity 401k" && (a.TaxBucket == nil || *a.TaxBucket != "none") {
			t.Errorf("stored tag %v", a.TaxBucket)
		}
	}
}

func TestTagAccount_Rejects(t *testing.T) {
	tagEnv(t)
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"Fidelity 401k", "ira"}, 2, "unknown bucket"},
		{[]string{"Fidelity 401k", "untagged"}, 2, "unknown bucket"},
		{[]string{"Savings", "cash"}, 1, "2 accounts are named"},
		{[]string{"Nope", "cash"}, 1, "no account is named"},
		{[]string{"Old HSA", "hsa"}, 1, "no account is named"},
		{[]string{"Fidelity 401k"}, 2, "expected"},
		{[]string{"a", "b", "c"}, 2, "expected"},
	} {
		code, out, errOut := runTag(c.args...)
		if code != c.code || !strings.Contains(errOut, c.want) || out != "" {
			t.Errorf("%v = %d %q %q", c.args, code, out, errOut)
		}
		if strings.Contains(errOut, "acct-") || strings.Contains(errOut, tagPlan) {
			t.Errorf("%v printed an id: %q", c.args, errOut)
		}
	}
	_, out, _ := runTag()
	if strings.Contains(out, "cash") || strings.Contains(out, "traditional") {
		t.Errorf("a rejected call tagged something:\n%s", out)
	}
}

func TestTagAccount_NeedsExistingDB(t *testing.T) {
	revokeEnv(t)
	t.Setenv("YNAB_BUDGET_ID", tagPlan)
	t.Setenv("FINBOT_DB_PATH", t.TempDir()+"/missing.db")
	if code, _, errOut := runTag(); code != 1 || !strings.Contains(errOut, "no database") {
		t.Errorf("= %d %q", code, errOut)
	}
}
