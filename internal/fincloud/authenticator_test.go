package fincloud

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func authResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestAuthenticatorLoginFieldsAndTemporaryLogout(t *testing.T) {
	logouts := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/admin/access/login":
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if request.Form.Get("username") != "USER001" || request.Form.Get("pwd") != "secret" || request.Form.Get("locationid") != "001" || request.Form.Get("roleid") != "R1" {
				t.Fatalf("form=%v", request.Form)
			}
			return authResponse(http.StatusOK, `{"status":"ok","data":{"result":{"sessionid":"temporary"}}}`), nil
		case "/admin/access/logout":
			if request.Header.Get("sessionid") != "temporary" {
				t.Fatalf("session=%q", request.Header.Get("sessionid"))
			}
			logouts++
			return authResponse(http.StatusOK, `{}`), nil
		default:
			t.Fatalf("path=%s", request.URL.Path)
			return nil, nil
		}
	})}
	authenticator, err := NewAuthenticator(AuthenticatorConfig{BaseURL: "https://fincloud.test", HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	if err := authenticator.Authenticate(context.Background(), "  USER001  ", "secret", "001", "R1"); err != nil {
		t.Fatal(err)
	}
	if logouts != 1 {
		t.Fatalf("logouts=%d", logouts)
	}
}

func TestAuthenticatorClassifiesCredentialsAndUpstreamFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		invalid bool
	}{{"invalid", http.StatusUnauthorized, `{}`, true}, {"explicit rejection", http.StatusOK, `{"status":"error","error":{"system":"user rejected"}}`, true}, {"unavailable", http.StatusServiceUnavailable, `{}`, false}, {"malformed", http.StatusOK, `{`, false}} {
		t.Run(test.name, func(t *testing.T) {
			httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return authResponse(test.status, test.body), nil })}
			authenticator, _ := NewAuthenticator(AuthenticatorConfig{BaseURL: "https://fincloud.test", HTTPClient: httpClient})
			err := authenticator.Authenticate(context.Background(), "user", "password", "1", "2")
			if errors.Is(err, ErrInvalidCredentials) != test.invalid {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAuthenticatorLoadsLabels(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/admin/access/listvalues" {
			t.Fatal(request.URL.Path)
		}
		return authResponse(http.StatusOK, `{"status":"ok","data":{"result":{"locationid":[{"id":"001","descr":"Branch"}],"roleid":[{"id":"R1","descr":"Teller"}]}}}`), nil
	})}
	authenticator, _ := NewAuthenticator(AuthenticatorConfig{BaseURL: "https://fincloud.test", HTTPClient: httpClient})
	labels, err := authenticator.Labels(context.Background())
	if err != nil || len(labels.Locations) != 1 || labels.Locations[0].ID != "001" || len(labels.Roles) != 1 {
		t.Fatalf("labels=%+v err=%v", labels, err)
	}
}

func TestAuthenticatorLogoutFailureIsBestEffort(t *testing.T) {
	var logs bytes.Buffer
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/admin/access/login" {
			return authResponse(http.StatusOK, `{"status":"ok","data":{"result":{"sessionid":"temporary"}}}`), nil
		}
		return authResponse(http.StatusServiceUnavailable, `{}`), nil
	})}
	authenticator, _ := NewAuthenticator(AuthenticatorConfig{
		BaseURL: "https://fincloud.test", HTTPClient: httpClient,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err := authenticator.Authenticate(context.Background(), "user", "secret", "1", "2"); err != nil {
		t.Fatalf("successful credentials became an error: %v", err)
	}
	if !strings.Contains(logs.String(), "clean up temporary Fincloud authentication session") {
		t.Fatalf("cleanup failure was not logged: %q", logs.String())
	}
}

func TestBrowserAuthenticatorDoesNotTouchSystemSessionManager(t *testing.T) {
	system, err := NewClient(Config{
		BaseURL: "https://fincloud.test", Username: "system", Password: "system-secret", LocationID: "system-location", RoleID: "system-role",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("system client was used by browser authentication")
			return nil, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	browser, _ := NewAuthenticator(AuthenticatorConfig{
		BaseURL: "https://fincloud.test",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/admin/access/login" {
				return authResponse(http.StatusOK, `{"status":"ok","data":{"result":{"sessionid":"browser-temporary"}}}`), nil
			}
			return authResponse(http.StatusOK, `{}`), nil
		})},
	})
	if err := browser.Authenticate(context.Background(), "branch-user", "secret", "branch", "teller"); err != nil {
		t.Fatal(err)
	}
	if system.sessions.session != "" || system.sessions.retired != "" {
		t.Fatalf("browser login changed system sessions: %+v", system.sessions)
	}
}
