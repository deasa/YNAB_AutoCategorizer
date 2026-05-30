package config

import (
	"os"
	"testing"
	"time"
)

// clearEnv unsets all config-related env vars to isolate tests.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"YNAB_ACCESS_TOKEN", "YNAB_BUDGET_ID",
		"LIBSQL_DATABASE_URL", "LIBSQL_AUTH_TOKEN",
		"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION",
		"OPENAI_API_KEY", "OPENAI_BASE_URL",
		"CONFIDENCE_THRESHOLD", "DRY_RUN", "RUN_INTERVAL", "MAX_TRANSACTIONS",
		"EXCLUDED_CATEGORY_KEYWORDS",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// setRequiredEnv sets the minimum required env vars for a valid config.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("YNAB_ACCESS_TOKEN", "test-token")
	t.Setenv("LIBSQL_DATABASE_URL", "libsql://test.turso.io")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "test-project")
}

func TestLoad_AllRequiredVars(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.YNABAccessToken != "test-token" {
		t.Errorf("YNABAccessToken = %q, want %q", cfg.YNABAccessToken, "test-token")
	}
	if cfg.LibSQLDatabaseURL != "libsql://test.turso.io" {
		t.Errorf("LibSQLDatabaseURL = %q, want %q", cfg.LibSQLDatabaseURL, "libsql://test.turso.io")
	}
	if cfg.GoogleCloudProject != "test-project" {
		t.Errorf("GoogleCloudProject = %q, want %q", cfg.GoogleCloudProject, "test-project")
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// README: YNAB_BUDGET_ID defaults to "last-used"
	if cfg.YNABBudgetID != "last-used" {
		t.Errorf("YNABBudgetID = %q, want %q", cfg.YNABBudgetID, "last-used")
	}

	// CONFIDENCE_THRESHOLD defaults to 0.75 (AI certainty threshold)
	if cfg.ConfidenceThreshold != 0.75 {
		t.Errorf("ConfidenceThreshold = %v, want %v", cfg.ConfidenceThreshold, 0.75)
	}

	// README: DRY_RUN defaults to false
	if cfg.DryRun != false {
		t.Errorf("DryRun = %v, want false", cfg.DryRun)
	}

	// README: RUN_INTERVAL defaults to 6h
	if cfg.RunInterval != 6*time.Hour {
		t.Errorf("RunInterval = %v, want %v", cfg.RunInterval, 6*time.Hour)
	}

	// README: GOOGLE_CLOUD_LOCATION defaults to us-central1
	if cfg.GoogleCloudLocation != "us-central1" {
		t.Errorf("GoogleCloudLocation = %q, want %q", cfg.GoogleCloudLocation, "us-central1")
	}
}

func TestLoad_MissingYNABAccessToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("LIBSQL_DATABASE_URL", "libsql://test.turso.io")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "test-project")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing YNAB_ACCESS_TOKEN, got nil")
	}
}

func TestLoad_MissingLibSQLDatabaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("YNAB_ACCESS_TOKEN", "test-token")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "test-project")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing LIBSQL_DATABASE_URL, got nil")
	}
}

func TestLoad_MissingAIProvider(t *testing.T) {
	clearEnv(t)
	t.Setenv("YNAB_ACCESS_TOKEN", "test-token")
	t.Setenv("LIBSQL_DATABASE_URL", "libsql://test.turso.io")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when no AI provider is configured, got nil")
	}
}

func TestLoad_OpenAIProviderOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv("YNAB_ACCESS_TOKEN", "test-token")
	t.Setenv("LIBSQL_DATABASE_URL", "libsql://test.turso.io")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenAIAPIKey != "sk-test" {
		t.Errorf("OpenAIAPIKey = %q, want %q", cfg.OpenAIAPIKey, "sk-test")
	}
}

func TestLoad_InvalidConfidenceThreshold(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("CONFIDENCE_THRESHOLD", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid CONFIDENCE_THRESHOLD, got nil")
	}
}

func TestLoad_InvalidRunInterval(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("RUN_INTERVAL", "not-a-duration")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid RUN_INTERVAL, got nil")
	}
}

func TestLoad_DryRunTrue(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("DRY_RUN", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.DryRun {
		t.Error("DryRun = false, want true")
	}
}

func TestLoad_CustomRunInterval(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("RUN_INTERVAL", "1h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.RunInterval != time.Hour {
		t.Errorf("RunInterval = %v, want %v", cfg.RunInterval, time.Hour)
	}
}

func TestLoad_DryRunNonTrue(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("DRY_RUN", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DryRun {
		t.Error("DryRun = true, want false for DRY_RUN=false")
	}
}

func TestLoad_DryRunArbitraryString(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("DRY_RUN", "yes")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DryRun {
		t.Error("DryRun = true, want false for DRY_RUN=yes (only 'true' enables dry run)")
	}
}

func TestLoad_CustomBudgetID(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("YNAB_BUDGET_ID", "my-budget-123")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.YNABBudgetID != "my-budget-123" {
		t.Errorf("YNABBudgetID = %q, want %q", cfg.YNABBudgetID, "my-budget-123")
	}
}

func TestLoad_OpenAIBaseURL(t *testing.T) {
	clearEnv(t)
	t.Setenv("YNAB_ACCESS_TOKEN", "test-token")
	t.Setenv("LIBSQL_DATABASE_URL", "libsql://test.turso.io")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", "https://custom.openai.com/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OpenAIBaseURL != "https://custom.openai.com/v1" {
		t.Errorf("OpenAIBaseURL = %q, want %q", cfg.OpenAIBaseURL, "https://custom.openai.com/v1")
	}
}

func TestLoad_BothAIProviders(t *testing.T) {
	clearEnv(t)
	t.Setenv("YNAB_ACCESS_TOKEN", "test-token")
	t.Setenv("LIBSQL_DATABASE_URL", "libsql://test.turso.io")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "test-project")
	t.Setenv("OPENAI_API_KEY", "sk-test")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GoogleCloudProject != "test-project" || cfg.OpenAIAPIKey != "sk-test" {
		t.Error("both AI providers should be loaded when both are set")
	}
}

func TestLoad_CustomGoogleCloudLocation(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("GOOGLE_CLOUD_LOCATION", "europe-west1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GoogleCloudLocation != "europe-west1" {
		t.Errorf("GoogleCloudLocation = %q, want %q", cfg.GoogleCloudLocation, "europe-west1")
	}
}

func TestLoad_MaxTransactions(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("MAX_TRANSACTIONS", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxTransactions != 5 {
		t.Errorf("MaxTransactions = %d, want 5", cfg.MaxTransactions)
	}
}

func TestLoad_MaxTransactions_DefaultsZero(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxTransactions != 0 {
		t.Errorf("MaxTransactions = %d, want 0", cfg.MaxTransactions)
	}
}

func TestLoad_InvalidMaxTransactions(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("MAX_TRANSACTIONS", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid MAX_TRANSACTIONS, got nil")
	}
}

func TestLoad_ExcludedCategoryKeywords_Default(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"Birthday", "Gift"}
	if len(cfg.ExcludedCategoryKeywords) != len(want) {
		t.Fatalf("ExcludedCategoryKeywords = %v, want %v", cfg.ExcludedCategoryKeywords, want)
	}
	for i, w := range want {
		if cfg.ExcludedCategoryKeywords[i] != w {
			t.Errorf("ExcludedCategoryKeywords[%d] = %q, want %q", i, cfg.ExcludedCategoryKeywords[i], w)
		}
	}
}

func TestLoad_ExcludedCategoryKeywords_Custom(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("EXCLUDED_CATEGORY_KEYWORDS", "Vacation, Holiday ,")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Empty entries trimmed/dropped; surrounding whitespace trimmed.
	want := []string{"Vacation", "Holiday"}
	if len(cfg.ExcludedCategoryKeywords) != len(want) {
		t.Fatalf("ExcludedCategoryKeywords = %v, want %v", cfg.ExcludedCategoryKeywords, want)
	}
	for i, w := range want {
		if cfg.ExcludedCategoryKeywords[i] != w {
			t.Errorf("ExcludedCategoryKeywords[%d] = %q, want %q", i, cfg.ExcludedCategoryKeywords[i], w)
		}
	}
}

func TestLoad_CustomConfidenceThreshold(t *testing.T) {
	clearEnv(t)
	setRequiredEnv(t)
	t.Setenv("CONFIDENCE_THRESHOLD", "0.9")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConfidenceThreshold != 0.9 {
		t.Errorf("ConfidenceThreshold = %v, want %v", cfg.ConfidenceThreshold, 0.9)
	}
}
