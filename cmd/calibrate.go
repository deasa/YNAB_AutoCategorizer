package cmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/deasa/YNAB_AutoCategorizer/calibrate"
	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/classify/jev"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// newCalibrateModel builds the model calibrate measures (tests replace it).
var newCalibrateModel = func(apiKey string) (classify.Model, string, error) {
	c, err := jev.New(apiKey)
	return c, jev.PinnedModel, err
}

// calibrateCmd measures the rules + Jev stack on the mirror's labelled history
// and writes calibration.json. It needs FINBOT_DB_PATH, YNAB_BUDGET_ID and
// JEV_API_KEY, never calls YNAB, and prints names and aggregates only.
func calibrateCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("finbot calibrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", 0, "evaluate only the newest N labelled transactions (0 = all); older ones still serve as history")
	batch := fs.Int("batch", calibrate.DefaultBatch, "transactions per model call")
	out := fs.String("out", calibrate.DefaultPath, "where to write calibration.json")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *limit < 0 || *batch < 1 {
		fmt.Fprintf(stderr, "finbot calibrate: bad arguments\n\n%s", usage)
		return 2
	}
	path := config.LoadDBPath()
	planID := os.Getenv("YNAB_BUDGET_ID")
	apiKey := os.Getenv("JEV_API_KEY")
	switch {
	case planID == "":
		fmt.Fprintln(stderr, "finbot calibrate: YNAB_BUDGET_ID is not set")
		return 1
	case apiKey == "":
		fmt.Fprintln(stderr, "finbot calibrate: JEV_API_KEY is not set")
		return 1
	}
	// store.Open would create an empty database at a wrong path.
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(stderr, "finbot calibrate: no database at FINBOT_DB_PATH")
		return 1
	}
	model, modelID, err := newCalibrateModel(apiKey)
	if err != nil {
		fmt.Fprintln(stderr, "finbot calibrate:", scrub(err.Error(), apiKey))
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintln(stderr, "finbot calibrate: open database:", scrub(err.Error(), planID))
		return 1
	}
	defer db.Close()
	err = runCalibrate(ctx, db, planID, *out, calibrate.Options{Model: model, ModelID: modelID, Limit: *limit, Batch: *batch}, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "finbot calibrate:", scrub(err.Error(), planID, apiKey))
		return 1
	}
	return 0
}

// runCalibrate reads the inputs, runs calibration and writes the result to out.
// opt's Policy and Candidates are filled here.
func runCalibrate(ctx context.Context, db *store.DB, planID, out string, opt calibrate.Options, stdout io.Writer) error {
	policy, err := classify.DefaultPolicy()
	if err != nil {
		return err
	}
	history, set, err := calibrate.Inputs(ctx, db, planID, policy)
	if err != nil {
		return err
	}
	for _, w := range set.Warnings {
		fmt.Fprintln(stdout, "warning:", w)
	}
	opt.Policy, opt.Candidates = policy, set.Candidates
	c, err := calibrate.Run(ctx, history, opt)
	if err != nil {
		return err
	}
	if err := calibrate.WriteFile(out, c); err != nil {
		return err
	}
	tau := "none"
	if c.Tau != nil {
		tau = fmt.Sprintf("%.2f", *c.Tau)
	}
	r := c.Report
	fmt.Fprintf(stdout, "calibrate: model=%s n=%d tau=%s precision=%.4f coverage=%.4f auto_eligible=%t\n",
		c.Model, c.N, tau, c.Precision, c.Coverage, c.AutoEligible)
	fmt.Fprintf(stdout, "  tune=%d (model %d) holdout=%d (model %d) rules=%d guard=%d rule_precision=%.4f top3=%.4f hard_negatives=%d (auto-applied %d)\n",
		r.TuneRows, r.TuneModelRows, r.HoldoutRows, r.HoldoutModelRows, r.Sources[calibrate.SourceRule],
		r.Sources[calibrate.SourceGuard], r.RulePrecision, r.Top3HitRate, r.HardNegatives, r.HardNegativesAutoApplied)
	fmt.Fprintf(stdout, "wrote %s\n", out)
	return nil
}
