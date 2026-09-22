package fincloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrInvalidCredentials = errors.New("invalid Fincloud credentials")

type AuthLabel struct {
	ID          string `json:"id"`
	Description string `json:"descr"`
}

type AuthLabels struct {
	Locations []AuthLabel `json:"locationid"`
	Roles     []AuthLabel `json:"roleid"`
}

type AuthenticatorConfig struct {
	BaseURL     string
	CAFile      string
	InsecureTLS bool
	Timeout     time.Duration
	HTTPClient  *http.Client
	Logger      *slog.Logger
}

type Authenticator struct {
	baseURL *url.URL
	http    *http.Client
	logger  *slog.Logger
}

func NewAuthenticator(config AuthenticatorConfig) (*Authenticator, error) {
	baseURL, err := url.Parse(strings.TrimRight(strings.TrimSpace(config.BaseURL), "/"))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, fmt.Errorf("FINCLOUD_BASE_URL must be an absolute URL without query or fragment")
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient, err = newHTTPClient(config.CAFile, config.InsecureTLS, config.Timeout)
		if err != nil {
			return nil, err
		}
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Authenticator{baseURL: baseURL, http: httpClient, logger: config.Logger}, nil
}

func (authenticator *Authenticator) Labels(ctx context.Context) (AuthLabels, error) {
	request, err := authenticator.request(ctx, http.MethodGet, "/admin/access/listvalues", nil)
	if err != nil {
		return AuthLabels{}, err
	}
	response, err := authenticator.http.Do(request)
	if err != nil {
		return AuthLabels{}, fmt.Errorf("load Fincloud authentication labels: %w", err)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, defaultJSONLimit)
	if err != nil {
		return AuthLabels{}, fmt.Errorf("read Fincloud authentication labels: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return AuthLabels{}, fmt.Errorf("Fincloud authentication labels returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result AuthLabels `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Status != "ok" {
		return AuthLabels{}, fmt.Errorf("decode Fincloud authentication labels")
	}
	return envelope.Data.Result, nil
}

func (authenticator *Authenticator) Authenticate(ctx context.Context, username, password, locationID, roleID string) error {
	form := url.Values{"username": {strings.TrimSpace(username)}, "pwd": {password}, "locationid": {strings.TrimSpace(locationID)}, "roleid": {strings.TrimSpace(roleID)}}
	request, err := authenticator.request(ctx, http.MethodPost, "/admin/access/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := authenticator.http.Do(request)
	if err != nil {
		return fmt.Errorf("Fincloud authentication unavailable: %w", err)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, defaultJSONLimit)
	if err != nil {
		return fmt.Errorf("read Fincloud authentication response: %w", err)
	}
	if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return ErrInvalidCredentials
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Fincloud authentication returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result struct {
				SessionID string `json:"sessionid"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode Fincloud authentication response: %w", err)
	}
	if envelope.Status != "ok" {
		return ErrInvalidCredentials
	}
	sessionID := strings.TrimSpace(envelope.Data.Result.SessionID)
	if sessionID == "" {
		return fmt.Errorf("Fincloud authentication response omitted session ID")
	}
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := authenticator.logout(cleanupContext, sessionID); err != nil {
		authenticator.logger.WarnContext(ctx, "clean up temporary Fincloud authentication session", "error", err)
	}
	return nil
}

func (authenticator *Authenticator) logout(ctx context.Context, sessionID string) error {
	request, err := authenticator.request(ctx, http.MethodPost, "/admin/access/logout", nil)
	if err != nil {
		return err
	}
	request.Header.Set("sessionid", sessionID)
	response, err := authenticator.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Fincloud logout returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (authenticator *Authenticator) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, authenticator.baseURL.JoinPath(path).String(), body)
	if err != nil {
		return nil, fmt.Errorf("build Fincloud authentication request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "THOR-Rate-Sync/1")
	return request, nil
}
