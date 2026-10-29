package sync

import (
	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
	ynabapi "pkg.venceslau.dev/ynab"
)

// batch is one export or delta mapped onto mirror rows.
type batch struct {
	accounts       []store.Account
	categoryGroups []store.CategoryGroup
	categories     []store.Category
	payees         []store.Payee
	transactions   []store.Transaction
	subs           []store.Subtransaction
	months         []store.Month
	monthCats      []store.MonthCategory
	scheduled      []store.ScheduledTransaction
	scheduledSubs  []store.ScheduledSubtransaction
}

func mapPlan(p *ynab.PlanDetail) *batch {
	b := &batch{}
	if p == nil {
		return b
	}
	for _, a := range p.Accounts {
		b.accounts = append(b.accounts, store.Account{
			ID: a.ID, Name: a.Name, Type: string(a.Type), OnBudget: a.OnBudget, Closed: a.Closed,
			Note: a.Note, Balance: int64(a.Balance), ClearedBalance: int64(a.ClearedBalance),
			UnclearedBalance: int64(a.UnclearedBalance), TransferPayeeID: a.TransferPayeeID,
			DirectImportLinked: a.DirectImportLinked, DirectImportInError: a.DirectImportInError,
			LastReconciledAt: a.LastReconciledAt, DebtOriginalBalance: milliPtr(a.DebtOriginalBalance),
			DebtInterestRates: periodic(a.DebtInterestRates), DebtMinimumPayments: periodic(a.DebtMinimumPayments),
			DebtEscrowAmounts: periodic(a.DebtEscrowAmounts), Deleted: a.Deleted,
		})
	}
	for _, g := range p.CategoryGroups {
		b.categoryGroups = append(b.categoryGroups, store.CategoryGroup{
			ID: g.ID, Name: g.Name, Hidden: g.Hidden, Internal: g.Internal, Deleted: g.Deleted,
		})
	}
	for _, c := range p.Categories {
		b.categories = append(b.categories, mapCategory(c))
	}
	for _, x := range p.Payees {
		b.payees = append(b.payees, store.Payee{
			ID: x.ID, Name: x.Name, TransferAccountID: x.TransferAccountID, Deleted: x.Deleted,
		})
	}
	for _, t := range p.Transactions {
		b.transactions = append(b.transactions, store.Transaction{
			ID: t.ID, Date: t.Date.String(), Amount: int64(t.Amount), Memo: t.Memo,
			Cleared: string(t.Cleared), Approved: t.Approved, FlagColor: strPtr(t.FlagColor),
			FlagName: t.FlagName, AccountID: t.AccountID, PayeeID: t.PayeeID, CategoryID: t.CategoryID,
			TransferAccountID: t.TransferAccountID, TransferTransactionID: t.TransferTransactionID,
			MatchedTransactionID: t.MatchedTransactionID, ImportID: t.ImportID,
			ImportPayeeName: t.ImportPayeeName, ImportPayeeNameOriginal: t.ImportPayeeNameOriginal,
			DebtTransactionType: strPtr(t.DebtTransactionType), Deleted: t.Deleted,
		})
	}
	for _, s := range p.Subtransactions {
		b.subs = append(b.subs, store.Subtransaction{
			ID: s.ID, TransactionID: s.TransactionID, Amount: int64(s.Amount), Memo: s.Memo,
			PayeeID: s.PayeeID, CategoryID: s.CategoryID, TransferAccountID: s.TransferAccountID,
			TransferTransactionID: s.TransferTransactionID, Deleted: s.Deleted,
		})
	}
	for _, m := range p.Months {
		month := m.Month.String()
		b.months = append(b.months, store.Month{
			Month: month, Note: m.Note, Income: int64(m.Income), Budgeted: int64(m.Budgeted),
			Activity: int64(m.Activity), ToBeBudgeted: int64(m.ToBeBudgeted),
			AgeOfMoney: intPtr(m.AgeOfMoney), Deleted: m.Deleted,
		})
		for _, c := range m.Categories {
			b.monthCats = append(b.monthCats, store.MonthCategory{
				Month: month, CategoryID: c.ID, Budgeted: int64(c.Budgeted), Activity: int64(c.Activity),
				Balance: int64(c.Balance), GoalTarget: milliPtr(c.GoalTarget),
				GoalUnderFunded: milliPtr(c.GoalUnderFunded), GoalOverallFunded: milliPtr(c.GoalOverallFunded),
				GoalOverallLeft: milliPtr(c.GoalOverallLeft), GoalPercentageComplete: intPtr(c.GoalPercentageComplete),
				Deleted: c.Deleted,
			})
		}
	}
	for _, s := range p.ScheduledTransactions {
		b.scheduled = append(b.scheduled, store.ScheduledTransaction{
			ID: s.ID, DateFirst: s.DateFirst.String(), DateNext: s.DateNext.String(),
			Frequency: string(s.Frequency), Amount: int64(s.Amount), Memo: s.Memo,
			FlagColor: strPtr(s.FlagColor), FlagName: s.FlagName, AccountID: s.AccountID,
			PayeeID: s.PayeeID, CategoryID: s.CategoryID, TransferAccountID: s.TransferAccountID,
			Deleted: s.Deleted,
		})
	}
	for _, s := range p.ScheduledSubtransactions {
		b.scheduledSubs = append(b.scheduledSubs, store.ScheduledSubtransaction{
			ID: s.ID, ScheduledTransactionID: s.ScheduledTransactionID, Amount: int64(s.Amount),
			Memo: s.Memo, PayeeID: s.PayeeID, CategoryID: s.CategoryID,
			TransferAccountID: s.TransferAccountID, Deleted: s.Deleted,
		})
	}
	return b
}

func mapCategory(c ynabapi.CategoryBase) store.Category {
	return store.Category{
		ID: c.ID, CategoryGroupID: c.CategoryGroupID, Name: c.Name, Hidden: c.Hidden, Internal: c.Internal,
		OriginalCategoryGroupID: c.OriginalCategoryGroupID, Note: c.Note, Budgeted: int64(c.Budgeted),
		Activity: int64(c.Activity), Balance: int64(c.Balance), GoalType: strPtr(c.GoalType),
		GoalNeedsWholeAmount: c.GoalNeedsWholeAmount, GoalDay: intPtr(c.GoalDay),
		GoalCadence: intPtr(c.GoalCadence), GoalCadenceFrequency: intPtr(c.GoalCadenceFrequency),
		GoalCreationMonth: datePtr(c.GoalCreationMonth), GoalTarget: milliPtr(c.GoalTarget),
		GoalTargetMonth: datePtr(c.GoalTargetMonth), GoalTargetDate: datePtr(c.GoalTargetDate),
		GoalPercentageComplete: intPtr(c.GoalPercentageComplete), GoalMonthsToBudget: intPtr(c.GoalMonthsToBudget),
		GoalUnderFunded: milliPtr(c.GoalUnderFunded), GoalOverallFunded: milliPtr(c.GoalOverallFunded),
		GoalOverallLeft: milliPtr(c.GoalOverallLeft), GoalSnoozedAt: c.GoalSnoozedAt, Deleted: c.Deleted,
	}
}

func milliPtr(m *ynabapi.Milliunits) *int64 {
	if m == nil {
		return nil
	}
	v := int64(*m)
	return &v
}

func intPtr(i *int) *int64 {
	if i == nil {
		return nil
	}
	v := int64(*i)
	return &v
}

func strPtr[S ~string](s *S) *string {
	if s == nil {
		return nil
	}
	v := string(*s)
	return &v
}

func datePtr(d *ynabapi.Date) *string {
	if d == nil {
		return nil
	}
	v := d.String()
	return &v
}

func periodic(p ynabapi.LoanAccountPeriodicValue) map[string]int64 {
	if len(p) == 0 {
		return nil
	}
	m := make(map[string]int64, len(p))
	for k, v := range p {
		m[k] = int64(v)
	}
	return m
}
