package config

import (
	"fmt"
	"os"
	"strconv"
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

	// App settings
	ConfidenceThreshold float64
	DryRun              bool
	RunInterval         time.Duration
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

	// Confidence threshold
	threshStr := getEnvDefault("CONFIDENCE_THRESHOLD", "0.7")
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
