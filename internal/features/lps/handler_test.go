package lps

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

	"github.com/go-chi/chi/v5"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/fincloud"
	corelps "github.com/ibldzn/trs/internal/lps"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	"github.com/ibldzn/trs/internal/platform/navigation"
	"github.com/ibldzn/trs/internal/render"
	webfiles "github.com/ibldzn/trs/web"
)

type failingGenerator struct{ input corelps.Input }

func (generator *failingGenerator) Generate(_ context.Context, input corelps.Input, _ io.Writer) (corelps.Result, error) {
	generator.input = input
	return corelps.Result{}, errors.New("source unavailable")
}

type fakeAuthentication struct{ principal browserauth.Principal }

func (*fakeAuthentication) Login(context.Context, browserauth.LoginInput, time.Time) (browserauth.LoginResult, error) {
	return browserauth.LoginResult{}, browserauth.ErrInvalidCredentials
}
func (*fakeAuthentication) Labels(context.Context) (fincloud.AuthLabels, error) {
	return fincloud.AuthLabels{}, nil
}
func (service *fakeAuthentication) ResolveSession(context.Context, [32]byte, time.Time) (browserauth.Principal, error) {
	return service.principal, nil
}
func (*fakeAuthentication) Logout(context.Context, [32]byte) error { return nil }

func TestGenerateAcceptsDateInputAndKeepsSelectionsOnFailure(t *testing.T) {
	generator := &failingGenerator{}
	renderer, err := render.New(webfiles.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	responder := render.NewErrorResponder(renderer, "Test", logger)
	registry, err := navigation.NewRegistry([]navigation.Group{{Key: "general", Label: "General", Items: []navigation.Item{Navigation()}}}, PermissionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	principal := browserauth.Principal{UserID: 1, Username: "viewer", Permissions: access.NewPermissionSet([]string{PermissionGenerate})}
	authentication := browserauth.NewHTTP(&fakeAuthentication{principal}, renderer, browserauth.NewCookieManager("session", false, time.Hour), "Test", logger, func(context.Context, audit.Event) error { return nil }, responder)
	router := chi.NewRouter()
	router.Use(authentication.LoadPrincipal)
	NewHandler(adminshell.New(renderer, registry, "Test", responder), generator, "31300082", time.UTC, nil, logger).RegisterRoutes(router)
	token, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{"participant_code": {"31300082"}, "reporting_date": {"2026-09-14"}, "period": {"M"}, "version": {"K1"}}
	request := httptest.NewRequest(http.MethodPost, "/lps", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "session", Value: token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if generator.input.ReportingDate != "20260914" {
		t.Fatalf("generator date = %q", generator.input.ReportingDate)
	}
	body := response.Body.String()
	for _, want := range []string{`type="date"`, `value="2026-09-14"`, `value="M" selected`, `value="K1" selected`} {
		if !strings.Contains(body, want) {
			t.Fatalf("status=%d missing %q in %q", response.Code, want, body)
		}
	}
}
