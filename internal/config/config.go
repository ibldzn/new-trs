package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const (
	defaultAppName                 = "THOR Rate Sync"
	defaultAppEnvironment          = "development"
	defaultAppURL                  = "http://localhost:8080"
	defaultAppPort                 = 8080
	defaultAppTimezone             = "Asia/Jakarta"
	defaultDatabaseHost            = "127.0.0.1"
	defaultDatabasePort            = 3306
	defaultSessionCookieName       = "trs_session"
	defaultSessionLifetime         = 24 * time.Hour
	defaultSessionRememberLifetime = 30 * 24 * time.Hour
	defaultSnapshotInterval        = 3 * time.Hour
	defaultFincloudTimeout         = 30 * time.Second
	defaultReportLimit             = 100 << 20
	defaultReportingConcurrency    = 8
	defaultReportingUploadLimit    = 2 << 20
)

type Config struct {
	App       AppConfig
	APIKey    string
	Database  DatabaseConfig
	Session   SessionConfig
	DWH       ExternalDatabaseConfig
	MSO       ExternalDatabaseConfig
	Fincloud  FincloudConfig
	Snapshot  SnapshotConfig
	Reporting ReportingConfig
	LPS       LPSConfig
}

type AppConfig struct {
	Name              string
	Environment       string
	URL               string
	Port              int
	AllowRegistration bool
	Timezone          string
}

func (c AppConfig) Location() (*time.Location, error) { return time.LoadLocation(c.Timezone) }

func (c AppConfig) Address() string {
	return ":" + strconv.Itoa(c.Port)
}

func (c AppConfig) IsDevelopment() bool {
	return c.Environment == "development"
}

type DatabaseConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
}

type SessionConfig struct {
	CookieName       string
	Lifetime         time.Duration
	RememberLifetime time.Duration
	Secure           bool
}

type ExternalDatabaseConfig struct {
	DSN               string
	DebtorTypeQuery   string
	InterestTypeQuery string
}

type FincloudConfig struct {
	BaseURL       string
	Username      string
	Password      string
	LocationID    string
	RoleID        string
	CAFile        string
	InsecureTLS   bool
	Timeout       time.Duration
	MaxReportSize int64
}

type SnapshotConfig struct {
	RefreshInterval time.Duration
	RefreshOnStart  bool
}

type ReportingConfig struct {
	Concurrency    int
	MaxUploadBytes int64
}

type LPSConfig struct{ DefaultParticipantCode string }

type lookupEnv func(string) (string, bool)

func Load() (Config, error) {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}

	return parse(os.LookupEnv)
}

func parse(lookup lookupEnv) (Config, error) {
	value := func(key, fallback string) string {
		if result, ok := lookup(key); ok {
			return result
		}
		return fallback
	}

	appPort, err := parsePort("APP_PORT", value("APP_PORT", strconv.Itoa(defaultAppPort)))
	if err != nil {
		return Config{}, err
	}
	databasePort, err := parsePort("DB_PORT", value("DB_PORT", strconv.Itoa(defaultDatabasePort)))
	if err != nil {
		return Config{}, err
	}
	allowRegistration, err := parseBool("ALLOW_REGISTRATION", value("ALLOW_REGISTRATION", "false"))
	if err != nil {
		return Config{}, err
	}
	sessionSecure, err := parseBool("SESSION_SECURE", value("SESSION_SECURE", "false"))
	if err != nil {
		return Config{}, err
	}
	sessionLifetime, err := parseDuration("SESSION_LIFETIME", value("SESSION_LIFETIME", defaultSessionLifetime.String()))
	if err != nil {
		return Config{}, err
	}
	rememberLifetime, err := parseDuration("SESSION_REMEMBER_LIFETIME", value("SESSION_REMEMBER_LIFETIME", defaultSessionRememberLifetime.String()))
	if err != nil {
		return Config{}, err
	}
	fincloudInsecureTLS, err := parseBool("FINCLOUD_INSECURE_TLS", value("FINCLOUD_INSECURE_TLS", "false"))
	if err != nil {
		return Config{}, err
	}
	snapshotInterval, err := parseDuration("TODAY_SNAPSHOT_REFRESH_INTERVAL", value("TODAY_SNAPSHOT_REFRESH_INTERVAL", defaultSnapshotInterval.String()))
	if err != nil {
		return Config{}, err
	}
	snapshotOnStart, err := parseBool("TODAY_SNAPSHOT_REFRESH_ON_START", value("TODAY_SNAPSHOT_REFRESH_ON_START", "true"))
	if err != nil {
		return Config{}, err
	}
	fincloudTimeout, err := parseDuration("FINCLOUD_HTTP_TIMEOUT", value("FINCLOUD_HTTP_TIMEOUT", defaultFincloudTimeout.String()))
	if err != nil {
		return Config{}, err
	}
	maxReportSize, err := parsePositiveInt64("FINCLOUD_MAX_REPORT_BYTES", value("FINCLOUD_MAX_REPORT_BYTES", strconv.FormatInt(defaultReportLimit, 10)))
	if err != nil {
		return Config{}, err
	}
	reportingConcurrency, err := parsePositiveInt("REPORTING_CONCURRENCY", value("REPORTING_CONCURRENCY", strconv.Itoa(defaultReportingConcurrency)), 64)
	if err != nil {
		return Config{}, err
	}
	reportingUploadLimit, err := parsePositiveInt64("REPORTING_MAX_UPLOAD_BYTES", value("REPORTING_MAX_UPLOAD_BYTES", strconv.FormatInt(defaultReportingUploadLimit, 10)))
	if err != nil {
		return Config{}, err
	}

	config := Config{
		APIKey: strings.TrimSpace(value("THOR_API_KEY", "")),
		App: AppConfig{
			Name:              strings.TrimSpace(value("APP_NAME", defaultAppName)),
			Environment:       strings.TrimSpace(value("APP_ENV", defaultAppEnvironment)),
			URL:               strings.TrimSpace(value("APP_URL", defaultAppURL)),
			Port:              appPort,
			AllowRegistration: allowRegistration,
			Timezone:          strings.TrimSpace(value("APP_TIMEZONE", defaultAppTimezone)),
		},
		Database: DatabaseConfig{
			Host:     strings.TrimSpace(value("DB_HOST", defaultDatabaseHost)),
			Port:     databasePort,
			Name:     strings.TrimSpace(value("DB_NAME", "")),
			User:     strings.TrimSpace(value("DB_USER", "")),
			Password: value("DB_PASSWORD", ""),
		},
		Session: SessionConfig{
			CookieName:       strings.TrimSpace(value("SESSION_COOKIE_NAME", defaultSessionCookieName)),
			Lifetime:         sessionLifetime,
			RememberLifetime: rememberLifetime,
			Secure:           sessionSecure,
		},
		DWH: ExternalDatabaseConfig{DSN: strings.TrimSpace(value("DWH_DBSTRING", ""))},
		MSO: ExternalDatabaseConfig{
			DSN: strings.TrimSpace(value("MSO_DBSTRING", "")), DebtorTypeQuery: strings.TrimSpace(value("MSO_DEBTOR_TYPE_QUERY", "")),
			InterestTypeQuery: strings.TrimSpace(value("MSO_INTEREST_TYPE_QUERY", "")),
		},
		Fincloud: FincloudConfig{
			BaseURL: strings.TrimSpace(value("FINCLOUD_BASE_URL", "")), Username: strings.TrimSpace(value("FINCLOUD_SYSTEM_USERNAME", "")),
			Password: value("FINCLOUD_SYSTEM_PASSWORD", ""), LocationID: strings.TrimSpace(value("FINCLOUD_SYSTEM_LOCATION_ID", "")),
			RoleID: strings.TrimSpace(value("FINCLOUD_SYSTEM_ROLE_ID", "")), CAFile: strings.TrimSpace(value("FINCLOUD_CA_FILE", "")),
			InsecureTLS: fincloudInsecureTLS, Timeout: fincloudTimeout, MaxReportSize: maxReportSize,
		},
		Snapshot:  SnapshotConfig{RefreshInterval: snapshotInterval, RefreshOnStart: snapshotOnStart},
		Reporting: ReportingConfig{Concurrency: reportingConcurrency, MaxUploadBytes: reportingUploadLimit},
		LPS:       LPSConfig{DefaultParticipantCode: strings.TrimSpace(value("LPS_DEFAULT_KODE_KEPESERTAAN", "31300082"))},
	}

	if err := validate(config); err != nil {
		return Config{}, err
	}

	return config, nil
}

func validate(config Config) error {
	required := []struct {
		key   string
		value string
	}{
		{"APP_NAME", config.App.Name},
		{"APP_ENV", config.App.Environment},
		{"APP_TIMEZONE", config.App.Timezone},
		{"DB_HOST", config.Database.Host},
		{"DB_NAME", config.Database.Name},
		{"DB_USER", config.Database.User},
		{"SESSION_COOKIE_NAME", config.Session.CookieName},
	}
	for _, field := range required {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.key)
		}
	}

	switch config.App.Environment {
	case "development", "production", "test":
	default:
		return fmt.Errorf("APP_ENV must be one of development, production, or test")
	}

	appURL, err := url.Parse(config.App.URL)
	if err != nil || appURL.Host == "" || (appURL.Scheme != "http" && appURL.Scheme != "https") {
		return fmt.Errorf("APP_URL must be an absolute http or https URL")
	}
	if _, err := time.LoadLocation(config.App.Timezone); err != nil {
		return fmt.Errorf("APP_TIMEZONE must name an installed timezone: %w", err)
	}

	return nil
}

func (config Config) ValidateServer() error {
	required := []struct{ key, value string }{
		{"DWH_DBSTRING", config.DWH.DSN}, {"MSO_DBSTRING", config.MSO.DSN},
		{"FINCLOUD_BASE_URL", config.Fincloud.BaseURL}, {"FINCLOUD_SYSTEM_USERNAME", config.Fincloud.Username},
		{"FINCLOUD_SYSTEM_PASSWORD", config.Fincloud.Password}, {"FINCLOUD_SYSTEM_LOCATION_ID", config.Fincloud.LocationID},
		{"FINCLOUD_SYSTEM_ROLE_ID", config.Fincloud.RoleID},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s must not be empty for server startup", field.key)
		}
	}
	parsed, err := url.Parse(config.Fincloud.BaseURL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return fmt.Errorf("FINCLOUD_BASE_URL must be an absolute https URL")
	}
	return nil
}

func parsePort(key, value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s must be an integer between 1 and 65535", key)
	}
	return port, nil
}

func parseBool(key, value string) (bool, error) {
	result, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return result, nil
}

func parseDuration(key, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return duration, nil
}

func parsePositiveInt(key, value string, maximum int) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 || parsed > maximum {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", key, maximum)
	}
	return parsed, nil
}

func parsePositiveInt64(key, value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}
