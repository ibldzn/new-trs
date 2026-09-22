package browserauth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/render"
	"github.com/ibldzn/trs/internal/user"
	webfiles "github.com/ibldzn/trs/web"
)

type fakeHTTPService struct {
	input  LoginInput
	result LoginResult
	err    error
}

func (*fakeHTTPService) Labels(context.Context) (fincloud.AuthLabels, error) {
	return fincloud.AuthLabels{
		Locations: []fincloud.AuthLabel{{ID: "001", Description: "Branch"}},
		Roles:     []fincloud.AuthLabel{{ID: "R1", Description: "Teller"}},
	}, nil
}
func (service *fakeHTTPService) Login(_ context.Context, input LoginInput, _ time.Time) (LoginResult, error) {
	service.input = input
	return service.result, service.err
}
func (*fakeHTTPService) ResolveSession(context.Context, [32]byte, time.Time) (Principal, error) {
	return Principal{}, ErrUnauthenticated
}
func (*fakeHTTPService) Logout(context.Context, [32]byte) error { return nil }

func TestLoginValidationPreservesFincloudSelections(t *testing.T) {
	service := &fakeHTTPService{}
	handler := newTestHTTP(t, service)
	form := url.Values{"username": {"  USER001  "}, "locationid": {"001"}, "roleid": {"R1"}, "remember_me": {"on"}}
	response := postLogin(handler, form)
	body := response.Body.String()
	for _, expected := range []string{`value="USER001"`, `value="001" selected`, `value="R1" selected`, "checked"} {
		if !strings.Contains(body, expected) {
			t.Errorf("response does not preserve %q", expected)
		}
	}
	if response.Code != http.StatusUnprocessableEntity || service.input.Username != "" {
		t.Fatalf("status=%d input=%+v", response.Code, service.input)
	}
}

func TestLoginErrorClassificationAndPasswordRedaction(t *testing.T) {
	for _, test := range []struct {
		name, password string
		err            error
		status         int
	}{
		{name: "credentials", password: "bad-secret", err: ErrInvalidCredentials, status: http.StatusUnprocessableEntity},
		{name: "upstream", password: "good-secret", err: errors.New("Fincloud unavailable"), status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeHTTPService{err: test.err}
			response := postLogin(newTestHTTP(t, service), validLoginForm(test.password))
			if response.Code != test.status || strings.Contains(response.Body.String(), test.password) || response.Header().Get("Set-Cookie") != "" {
				t.Fatalf("status=%d cookie=%q body=%q", response.Code, response.Header().Get("Set-Cookie"), response.Body.String())
			}
		})
	}
}

func TestLoginCreatesTHORCookieAndUsesAuthorizedDefault(t *testing.T) {
	token, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeHTTPService{result: LoginResult{
		RawToken: token,
		Session:  auth.Session{RememberMe: true, CreatedAt: time.Now()},
		User:     user.User{ID: 7, Username: "user001"},
	}}
	form := validLoginForm("fincloud-secret")
	form.Set("remember_me", "on")
	response := postLogin(newTestHTTP(t, service), form)
	cookies := response.Result().Cookies()
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/loans/inquiry" || len(cookies) != 1 || cookies[0].Value != token || service.input.Password != "fincloud-secret" || !service.input.RememberMe {
		t.Fatalf("status=%d location=%q cookies=%+v input=%+v", response.Code, response.Header().Get("Location"), cookies, service.input)
	}
}

func TestSafeRedirectDefaultsToLoanInquiry(t *testing.T) {
	for input, expected := range map[string]string{"": "/loans/inquiry", "https://evil.example": "/loans/inquiry", "//evil.example": "/loans/inquiry", "/slik?mine=1": "/slik?mine=1"} {
		if actual := SafeRedirect(input); actual != expected {
			t.Errorf("SafeRedirect(%q)=%q want %q", input, actual, expected)
		}
	}
}

func validLoginForm(password string) url.Values {
	return url.Values{"username": {"USER001"}, "password": {password}, "locationid": {"001"}, "roleid": {"R1"}}
}

func postLogin(handler *HTTP, form url.Values) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.Login(response, request)
	return response
}

func newTestHTTP(t *testing.T, service authenticationService) *HTTP {
	t.Helper()
	renderer, err := render.New(webfiles.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHTTP(service, renderer, NewCookieManager("test_session", false, 30*24*time.Hour), "THOR", logger, nil, render.NewErrorResponder(renderer, "THOR", logger))
}
