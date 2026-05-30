package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	// YNAB API
	YNABAccessToken string
	YNABBudgetID    string

	// Turso DB
	LibSQLDatabaseURL string
	LibSQLAuthToken   string

	// AI Provider
	GoogleCloudProject  string
	GoogleCloudLocation string
	OpenAIAPIKey        string
	OpenAIBaseURL       string

	// Chat model override (empty = provider default)
	ChatModel string

	// App settings
	ConfidenceThreshold float64 // AI certainty threshold (0.0–1.0); default 0.75
	DryRun              bool
	RunInterval         time.Duration
	MaxTransactions     int // max uncategorized txns to process per run; 0 = unlimited

	// ExcludedCategoryKeywords: categories whose name contains any of these
	// (case-insensitive) are never auto-applied. Default: Birthday, Gift.
	ExcludedCategoryKeywords []string
}

// Load reads configuration from environment variables, returning an error
// if any required value is missing.
func Load() (*Config, error) {
	// Load .env if present; not required (env vars may be set directly).
	_ = godotenv.Load()

	cfg := &Config{
		YNABAccessToken:     os.Getenv("YNAB_ACCESS_TOKEN"),
		YNABBudgetID:        getEnvDefault("YNAB_BUDGET_ID", "last-used"),
		LibSQLDatabaseURL:   os.Getenv("LIBSQL_DATABASE_URL"),
		LibSQLAuthToken:     os.Getenv("LIBSQL_AUTH_TOKEN"),
		GoogleCloudProject:  os.Getenv("GOOGLE_CLOUD_PROJECT"),
		GoogleCloudLocation: getEnvDefault("GOOGLE_CLOUD_LOCATION", "us-central1"),
		OpenAIAPIKey:        os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:       os.Getenv("OPENAI_BASE_URL"),
	}

	// Chat model override
	cfg.ChatModel = os.Getenv("CHAT_MODEL")

	// Confidence threshold (AI certainty; default 0.75)
	threshStr := getEnvDefault("CONFIDENCE_THRESHOLD", "0.75")
	threshold, err := strconv.ParseFloat(threshStr, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid CONFIDENCE_THRESHOLD %q: %w", threshStr, err)
	}
	cfg.ConfidenceThreshold = threshold

	// Dry run
	cfg.DryRun = os.Getenv("DRY_RUN") == "true"

	// Run interval
	intervalStr := getEnvDefault("RUN_INTERVAL", "6h")
	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return nil, fmt.Errorf("invalid RUN_INTERVAL %q: %w", intervalStr, err)
	}
	cfg.RunInterval = interval

	// Max transactions per run (0 = unlimited)
	maxTxnStr := getEnvDefault("MAX_TRANSACTIONS", "0")
	maxTxn, err := strconv.Atoi(maxTxnStr)
	if err != nil {
		return nil, fmt.Errorf("invalid MAX_TRANSACTIONS %q: %w", maxTxnStr, err)
	}
	cfg.MaxTransactions = maxTxn

	// Excluded category keywords (case-insensitive substring match; default Birthday, Gift)
	excludedStr := getEnvDefault("EXCLUDED_CATEGORY_KEYWORDS", "Birthday,Gift")
	for _, kw := range strings.Split(excludedStr, ",") {
		if kw = strings.TrimSpace(kw); kw != "" {
			cfg.ExcludedCategoryKeywords = append(cfg.ExcludedCategoryKeywords, kw)
		}
	}

	// Validate required fields
	if cfg.YNABAccessToken == "" {
		return nil, fmt.Errorf("YNAB_ACCESS_TOKEN is required")
	}
	if cfg.LibSQLDatabaseURL == "" {
		return nil, fmt.Errorf("LIBSQL_DATABASE_URL is required")
	}
	if cfg.GoogleCloudProject == "" && cfg.OpenAIAPIKey == "" {
		return nil, fmt.Errorf("either GOOGLE_CLOUD_PROJECT or OPENAI_API_KEY is required")
	}

	return cfg, nil
}

func getEnvDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
