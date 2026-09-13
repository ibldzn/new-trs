package fincloud

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

const (
	defaultJSONLimit   = int64(8 << 20)
	defaultReportLimit = int64(100 << 20)
)

type Config struct {
	BaseURL       string
	Username      string
	Password      string
	LocationID    string
	RoleID        string
	CAFile        string
	InsecureTLS   bool
	Timeout       time.Duration
	MaxReportSize int64
	HTTPClient    *http.Client
}

type Client struct {
	baseURL       *url.URL
	http          *http.Client
	username      string
	password      string
	locationID    string
	roleID        string
	maxReportSize int64
	sessions      *SessionManager
}

func NewClient(config Config) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimRight(strings.TrimSpace(config.BaseURL), "/"))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, fmt.Errorf("FINCLOUD_BASE_URL must be an absolute URL without query or fragment")
	}
	if strings.TrimSpace(config.Username) == "" || config.Password == "" || strings.TrimSpace(config.LocationID) == "" || strings.TrimSpace(config.RoleID) == "" {
		return nil, fmt.Errorf("Fincloud system credentials, location, and role are required")
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	if config.MaxReportSize <= 0 {
		config.MaxReportSize = defaultReportLimit
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: config.InsecureTLS} //nolint:gosec // explicit emergency setting; false by default
		if config.CAFile != "" {
			certificate, err := os.ReadFile(config.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read Fincloud CA file: %w", err)
			}
			pool, err := x509.SystemCertPool()
			if err != nil {
				return nil, fmt.Errorf("load system CA pool: %w", err)
			}
			if !pool.AppendCertsFromPEM(certificate) {
				return nil, fmt.Errorf("FINCLOUD_CA_FILE contains no valid certificates")
			}
			tlsConfig.RootCAs = pool
		}
		httpClient = &http.Client{
			Timeout: config.Timeout,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsConfig,
				MaxIdleConns: 50, MaxIdleConnsPerHost: 20, MaxConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: config.Timeout,
			},
		}
	}
	client := &Client{
		baseURL: baseURL, http: httpClient, username: strings.TrimSpace(config.Username), password: config.Password,
		locationID: strings.TrimSpace(config.LocationID), roleID: strings.TrimSpace(config.RoleID), maxReportSize: config.MaxReportSize,
	}
	client.sessions, err = NewSessionManager(client.login, client.logout)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func (client *Client) Close(ctx context.Context) error { return client.sessions.Close(ctx) }

func (client *Client) login(ctx context.Context) (string, error) {
	form := url.Values{
		"locationid": {client.locationID}, "roleid": {client.roleID}, "username": {client.username}, "pwd": {client.password},
	}
	request, err := client.newRequest(ctx, http.MethodPost, "/admin/access/login", nil, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.http.Do(request)
	if err != nil {
		return "", errors.Join(loan.ErrFincloudUnavailable, err)
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body, defaultJSONLimit)
	if err != nil {
		return "", errors.Join(loan.ErrFincloudUnavailable, err)
	}
	if response.StatusCode >= 500 {
		return "", fmt.Errorf("%w: login returned HTTP %d", loan.ErrFincloudUnavailable, response.StatusCode)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusBadRequest {
		return "", loan.ErrFincloudCredentials
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: login returned HTTP %d", loan.ErrFincloudUnavailable, response.StatusCode)
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
		return "", errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("decode login response: %w", err))
	}
	if envelope.Status != "ok" {
		if isCredentialResponse(body) {
			return "", loan.ErrFincloudCredentials
		}
		return "", errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("Fincloud login was rejected without a credential error"))
	}
	if strings.TrimSpace(envelope.Data.Result.SessionID) == "" {
		return "", errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("Fincloud login response omitted session ID"))
	}
	return strings.TrimSpace(envelope.Data.Result.SessionID), nil
}

func (client *Client) logout(ctx context.Context, session string) error {
	request, err := client.newRequest(ctx, http.MethodPost, "/admin/access/logout", nil, nil)
	if err != nil {
		return err
	}
	request.Header.Set("sessionid", session)
	response, err := client.http.Do(request)
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

type requestBuilder func(context.Context, string) (*http.Request, error)

func (client *Client) do(ctx context.Context, limit int64, build requestBuilder) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		session, err := client.sessions.Session(ctx)
		if err != nil {
			return nil, err
		}
		request, err := build(ctx, session)
		if err != nil {
			return nil, err
		}
		request.Header.Set("sessionid", session)
		response, err := client.http.Do(request)
		if err != nil {
			return nil, errors.Join(loan.ErrFincloudUnavailable, err)
		}
		body, readErr := readBounded(response.Body, limit)
		closeErr := response.Body.Close()
		if readErr != nil {
			return nil, errors.Join(loan.ErrFincloudUnavailable, readErr)
		}
		if closeErr != nil {
			return nil, errors.Join(loan.ErrFincloudUnavailable, closeErr)
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || isAuthenticationResponse(body) {
			client.sessions.Invalidate(session)
			if attempt == 0 {
				continue
			}
			return nil, loan.ErrFincloudSession
		}
		if response.StatusCode == http.StatusNotFound || isNotFoundResponse(body) {
			return nil, loan.ErrNotFound
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, fmt.Errorf("%w: HTTP %d", loan.ErrFincloudUnavailable, response.StatusCode)
		}
		return body, nil
	}
	return nil, loan.ErrFincloudSession
}

func (client *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	reference, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("build Fincloud URL: %w", err)
	}
	target := client.baseURL.ResolveReference(reference)
	if query != nil {
		target.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build Fincloud request: %w", err)
	}
	request.Header.Set("Accept", "application/json, text/csv, text/plain")
	request.Header.Set("User-Agent", "THOR-Rate-Sync/1")
	return request, nil
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("response limit must be positive")
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", limit)
	}
	return body, nil
}

func isAuthenticationResponse(body []byte) bool {
	message, status := responseMessage(body)
	if status == "ok" {
		return false
	}
	message = strings.ToLower(message)
	return strings.Contains(message, "not logged") || strings.Contains(message, "not login") ||
		strings.Contains(message, "login required") || strings.Contains(message, "authentication required") ||
		strings.Contains(message, "unauthorized") || (strings.Contains(message, "session") &&
		(strings.Contains(message, "expired") || strings.Contains(message, "invalid") || strings.Contains(message, "timeout") || strings.Contains(message, "not found")))
}

func isCredentialResponse(body []byte) bool {
	message, _ := responseMessage(body)
	message = strings.ToLower(message)
	credential := strings.Contains(message, "password") || strings.Contains(message, "username") || strings.Contains(message, "credential") || strings.Contains(message, "login")
	rejected := strings.Contains(message, "invalid") || strings.Contains(message, "wrong") || strings.Contains(message, "incorrect") || strings.Contains(message, "reject")
	return credential && rejected
}

func isNotFoundResponse(body []byte) bool {
	message, status := responseMessage(body)
	return status != "ok" && strings.EqualFold(strings.TrimSpace(message), "Data not found")
}

func responseMessage(body []byte) (string, string) {
	var envelope struct {
		Status      string `json:"status"`
		Description string `json:"description"`
		Error       struct {
			System string `json:"system"`
		} `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(body), &envelope) != nil {
		return "", ""
	}
	return strings.TrimSpace(envelope.Description + " " + envelope.Error.System), envelope.Status
}
