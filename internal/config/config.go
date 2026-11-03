package config

import (
	"errors"
	"fmt"
	"net"
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
	defaultAppShutdownTimeout      = 45 * time.Second
	defaultAppTimezone             = "Asia/Jakarta"
	defaultDatabaseHost            = "127.0.0.1"
	defaultDatabasePort            = 3306
	defaultSessionCookieName       = "trs_session"
	defaultSessionLifetime         = 24 * time.Hour
	defaultSessionRememberLifetime = 30 * 24 * time.Hour
	defaultSnapshotInterval        = 3 * time.Hour
	defaultFincloudTimeout         = 30 * time.Second
	defaultReportLimit             = 100 << 20
	defaultSLIKConcurrency         = 16
	defaultSLIKUploadLimit         = 64 << 20
)

type Config struct {
	App                     AppConfig
	APIKey                  string
	BootstrapAccessManagers string
	Database                DatabaseConfig
	Session                 SessionConfig
	Auth                    AuthConfig
	DWH                     ExternalDatabaseConfig
	MSO                     ExternalDatabaseConfig
	Fincloud                FincloudConfig
	Snapshot                SnapshotConfig
	SLIK                    SLIKConfig
	LPS                     LPSConfig
}

type AppConfig struct {
	Name        string
	Environment string
	URL         string
	// BindHost empty listens on every interface.
	BindHost        string
	Port            int
	ShutdownTimeout time.Duration
	Timezone        string
	// Branding image paths are optional; see internal/branding.
	LogoFile     string
	WideLogoFile string
	FaviconFile  string
}

func (c AppConfig) Location() (*time.Location, error) { return time.LoadLocation(c.Timezone) }

func (c AppConfig) Address() string {
	return net.JoinHostPort(c.BindHost, strconv.Itoa(c.Port))
}

func (c AppConfig) IsDevelopment() bool {
	return c.Environment == "development"
}

type DatabaseConfig struct {
	Network  string
	Host     string
	Port     int
	Socket   string
	Name     string
	User     string
	Password string
}

// AuthConfig bounds failed browser logins per username before Fincloud is asked again.
type AuthConfig struct {
	LoginFailureWindow time.Duration
	LoginMaxFailures   int
	LoginLockout       time.Duration
}

type SessionConfig struct {
	CookieName       string
	Lifetime         time.Duration
	RememberLifetime time.Duration
	Secure           bool
}

type ExternalDatabaseConfig struct {
	DSN string
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

type SLIKConfig struct {
	Concurrency    int
	MaxUploadBytes int64
	StorageDir     string
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
	databaseNetwork := strings.TrimSpace(value("DB_NETWORK", "tcp"))
	databasePort := 0
	if databaseNetwork == "tcp" {
		databasePort, err = parsePort("DB_PORT", value("DB_PORT", strconv.Itoa(defaultDatabasePort)))
		if err != nil {
			return Config{}, err
		}
	}
	shutdownTimeout, err := parseDuration("APP_SHUTDOWN_TIMEOUT", value("APP_SHUTDOWN_TIMEOUT", defaultAppShutdownTimeout.String()))
	if err != nil {
		return Config{}, err
	}
	maxFailures, err := parsePositiveInt("AUTH_LOGIN_MAX_FAILURES", value("AUTH_LOGIN_MAX_FAILURES", "5"), 1000)
	if err != nil {
		return Config{}, err
	}
	failureWindow, err := parseDuration("AUTH_LOGIN_FAILURE_WINDOW", value("AUTH_LOGIN_FAILURE_WINDOW", "15m"))
	if err != nil {
		return Config{}, err
	}
	lockout, err := parseDuration("AUTH_LOGIN_LOCKOUT", value("AUTH_LOGIN_LOCKOUT", "15m"))
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
	slikConcurrency, err := parsePositiveInt("SLIK_CONCURRENCY", value("SLIK_CONCURRENCY", strconv.Itoa(defaultSLIKConcurrency)), 32)
	if err != nil {
		return Config{}, err
	}
	slikUploadLimit, err := parsePositiveInt64("SLIK_MAX_UPLOAD_BYTES", value("SLIK_MAX_UPLOAD_BYTES", strconv.FormatInt(defaultSLIKUploadLimit, 10)))
	if err != nil {
		return Config{}, err
	}
	if slikUploadLimit > 256<<20 {
		return Config{}, fmt.Errorf("SLIK_MAX_UPLOAD_BYTES must not exceed 268435456")
	}

	config := Config{
		APIKey:                  strings.TrimSpace(value("THOR_API_KEY", "")),
		BootstrapAccessManagers: strings.TrimSpace(value("THOR_BOOTSTRAP_ACCESS_MANAGERS", "")),
		App: AppConfig{
			Name: strings.TrimSpace(value("APP_NAME", defaultAppName)), Environment: strings.TrimSpace(value("APP_ENV", defaultAppEnvironment)),
			URL: strings.TrimSpace(value("APP_URL", defaultAppURL)), BindHost: strings.TrimSpace(value("APP_BIND_HOST", "")), Port: appPort,
			ShutdownTimeout: shutdownTimeout, Timezone: strings.TrimSpace(value("APP_TIMEZONE", defaultAppTimezone)),
			LogoFile: strings.TrimSpace(value("APP_LOGO_FILE", "")), WideLogoFile: strings.TrimSpace(value("APP_LOGO_WIDE_FILE", "")),
			FaviconFile: strings.TrimSpace(value("APP_FAVICON_FILE", "")),
		},
		Auth: AuthConfig{LoginFailureWindow: failureWindow, LoginMaxFailures: maxFailures, LoginLockout: lockout},
		Database: DatabaseConfig{
			Network:  databaseNetwork,
			Host:     strings.TrimSpace(value("DB_HOST", defaultDatabaseHost)),
			Port:     databasePort,
			Socket:   strings.TrimSpace(value("DB_SOCKET", "")),
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
		MSO: ExternalDatabaseConfig{DSN: strings.TrimSpace(value("MSO_DBSTRING", ""))},
		Fincloud: FincloudConfig{
			BaseURL: strings.TrimSpace(value("FINCLOUD_BASE_URL", "")), Username: strings.TrimSpace(value("FINCLOUD_SYSTEM_USERNAME", "")),
			Password: value("FINCLOUD_SYSTEM_PASSWORD", ""), LocationID: strings.TrimSpace(value("FINCLOUD_SYSTEM_LOCATION_ID", "")),
			RoleID: strings.TrimSpace(value("FINCLOUD_SYSTEM_ROLE_ID", "")), CAFile: strings.TrimSpace(value("FINCLOUD_CA_FILE", "")),
			InsecureTLS: fincloudInsecureTLS, Timeout: fincloudTimeout, MaxReportSize: maxReportSize,
		},
		Snapshot: SnapshotConfig{RefreshInterval: snapshotInterval, RefreshOnStart: snapshotOnStart},
		SLIK:     SLIKConfig{Concurrency: slikConcurrency, MaxUploadBytes: slikUploadLimit, StorageDir: strings.TrimSpace(value("SLIK_STORAGE_DIR", "./data/slik"))},
		LPS:      LPSConfig{DefaultParticipantCode: strings.TrimSpace(value("LPS_DEFAULT_KODE_KEPESERTAAN", "31300082"))},
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
		{"DB_NAME", config.Database.Name},
		{"DB_USER", config.Database.User},
		{"SESSION_COOKIE_NAME", config.Session.CookieName},
		{"SLIK_STORAGE_DIR", config.SLIK.StorageDir},
	}
	for _, field := range required {
		if field.value == "" {
			return fmt.Errorf("%s must not be empty", field.key)
		}
	}

	switch config.Database.Network {
	case "tcp":
		if config.Database.Host == "" {
			return fmt.Errorf("DB_HOST must not be empty")
		}
	case "unix":
		if config.Database.Socket == "" {
			return fmt.Errorf("DB_SOCKET must not be empty when DB_NETWORK=unix")
		}
	default:
		return fmt.Errorf("DB_NETWORK must be one of tcp or unix")
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
