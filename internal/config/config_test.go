package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	config, err := parse(mapLookup(map[string]string{
		"DB_NAME": "go_admin",
		"DB_USER": "root",
	}))
	if err != nil {
		t.Fatal(err)
	}

	if config.App.Name != "THOR Rate Sync" || config.App.Port != 8080 || config.App.Timezone != "Asia/Jakarta" || !config.App.IsDevelopment() {
		t.Fatalf("unexpected app defaults: %+v", config.App)
	}
	if config.Database.Host != "127.0.0.1" || config.Database.Port != 3306 {
		t.Fatalf("unexpected database defaults: %+v", config.Database)
	}
	if config.Session.Lifetime != 24*time.Hour || config.Session.RememberLifetime != 30*24*time.Hour {
		t.Fatalf("unexpected session defaults: %+v", config.Session)
	}
	if config.APIKey != "" {
		t.Fatalf("API should be disabled by default")
	}
	if config.Snapshot.RefreshInterval != 3*time.Hour || !config.Snapshot.RefreshOnStart || config.SLIK.Concurrency != 16 || config.SLIK.MaxUploadBytes != 64<<20 || config.SLIK.StorageDir != "./data/slik" {
		t.Fatalf("unexpected integration defaults: snapshot=%+v SLIK=%+v", config.Snapshot, config.SLIK)
	}
}

func TestParseAPIKey(t *testing.T) {
	config, err := parse(mapLookup(baseValues("THOR_API_KEY", "example-secret")))
	if err != nil || config.APIKey != "example-secret" {
		t.Fatalf("API key was not loaded: %v", err)
	}
}

func TestParseValidation(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]string
		wantErr string
	}{
		{"missing database name", map[string]string{"DB_USER": "root"}, "DB_NAME"},
		{"missing database user", map[string]string{"DB_NAME": "go_admin"}, "DB_USER"},
		{"invalid app port", baseValues("APP_PORT", "70000"), "APP_PORT"},
		{"invalid database port", baseValues("DB_PORT", "mysql"), "DB_PORT"},
		{"invalid app url", baseValues("APP_URL", "localhost:8080"), "APP_URL"},
		{"invalid app environment", baseValues("APP_ENV", "staging"), "APP_ENV"},
		{"invalid registration flag", baseValues("ALLOW_REGISTRATION", "sometimes"), "ALLOW_REGISTRATION"},
		{"invalid secure flag", baseValues("SESSION_SECURE", "sometimes"), "SESSION_SECURE"},
		{"invalid session lifetime", baseValues("SESSION_LIFETIME", "0s"), "SESSION_LIFETIME"},
		{"invalid remember lifetime", baseValues("SESSION_REMEMBER_LIFETIME", "later"), "SESSION_REMEMBER_LIFETIME"},
		{"invalid timezone", baseValues("APP_TIMEZONE", "Mars/Olympus"), "APP_TIMEZONE"},
		{"invalid snapshot interval", baseValues("TODAY_SNAPSHOT_REFRESH_INTERVAL", "0s"), "TODAY_SNAPSHOT_REFRESH_INTERVAL"},
		{"invalid snapshot start", baseValues("TODAY_SNAPSHOT_REFRESH_ON_START", "sometimes"), "TODAY_SNAPSHOT_REFRESH_ON_START"},
		{"invalid SLIK concurrency", baseValues("SLIK_CONCURRENCY", "100"), "SLIK_CONCURRENCY"},
		{"invalid SLIK upload limit", baseValues("SLIK_MAX_UPLOAD_BYTES", "0"), "SLIK_MAX_UPLOAD_BYTES"},
		{"excessive SLIK upload limit", baseValues("SLIK_MAX_UPLOAD_BYTES", "268435457"), "SLIK_MAX_UPLOAD_BYTES"},
		{"missing SLIK storage", baseValues("SLIK_STORAGE_DIR", ""), "SLIK_STORAGE_DIR"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parse(mapLookup(test.values))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected error containing %q, got %v", test.wantErr, err)
			}
		})
	}
}

func TestValidateServerRequiresIntegrationConfiguration(t *testing.T) {
	config, err := parse(mapLookup(baseValues("APP_ENV", "test")))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ValidateServer(); err == nil || !strings.Contains(err.Error(), "DWH_DBSTRING") {
		t.Fatalf("error = %v", err)
	}
	config.DWH.DSN = "dwh"
	config.MSO.DSN = "mso"
	config.Fincloud = FincloudConfig{BaseURL: "https://fincloud.example", Username: "system", Password: "secret", LocationID: "000", RoleID: "R-1"}
	if err := config.ValidateServer(); err != nil {
		t.Fatal(err)
	}
	config.Fincloud.BaseURL = "http://fincloud.example"
	if err := config.ValidateServer(); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseSupportedEnvironments(t *testing.T) {
	for _, environment := range []string{"development", "production", "test"} {
		t.Run(environment, func(t *testing.T) {
			config, err := parse(mapLookup(baseValues("APP_ENV", environment)))
			if err != nil {
				t.Fatal(err)
			}
			if config.App.Environment != environment {
				t.Fatalf("expected %q, got %q", environment, config.App.Environment)
			}
		})
	}
}

func TestLoadEnvironmentOverridesDotEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", []byte("DB_NAME=from_file\nDB_USER=file_user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_NAME", "from_environment")
	t.Setenv("DB_USER", "environment_user")

	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.Database.Name != "from_environment" || config.Database.User != "environment_user" {
		t.Fatalf("environment did not win: %+v", config.Database)
	}
}

func baseValues(key, value string) map[string]string {
	values := map[string]string{"DB_NAME": "go_admin", "DB_USER": "root"}
	values[key] = value
	return values
}

func mapLookup(values map[string]string) lookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
