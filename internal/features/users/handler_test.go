package users

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	"github.com/ibldzn/trs/internal/platform/navigation"
	"github.com/ibldzn/trs/internal/render"
	webfiles "github.com/ibldzn/trs/web"
)

type routeAuthentication struct{ principal browserauth.Principal }

func (routeAuthentication) Labels(context.Context) (fincloud.AuthLabels, error) {
	return fincloud.AuthLabels{}, nil
}
func (routeAuthentication) Login(context.Context, browserauth.LoginInput, time.Time) (browserauth.LoginResult, error) {
	return browserauth.LoginResult{}, browserauth.ErrInvalidCredentials
}
func (authentication routeAuthentication) ResolveSession(context.Context, [32]byte, time.Time) (browserauth.Principal, error) {
	return authentication.principal, nil
}
func (routeAuthentication) Logout(context.Context, [32]byte) error { return nil }

func TestAccessRoutesRequireAccessManage(t *testing.T) {
	for _, test := range []struct {
		name        string
		permissions []string
		status      int
	}{
		{name: "denied", status: http.StatusForbidden},
		{name: "allowed", permissions: []string{PermissionManage}, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, token := accessRouter(t, access.NewPermissionSet(test.permissions))
			request := httptest.NewRequest(http.MethodGet, "/access", nil)
			request.AddCookie(&http.Cookie{Name: "session", Value: token})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func accessRouter(t *testing.T, permissions access.PermissionSet) (http.Handler, string) {
	t.Helper()
	renderer, err := render.New(webfiles.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	errors := render.NewErrorResponder(renderer, "THOR", logger)
	definitions := accessDefinitions()
	registry, err := navigation.NewRegistry([]navigation.Group{{Key: "management", Label: "Management", Items: []navigation.Item{Navigation()}}}, definitions)
	if err != nil {
		t.Fatal(err)
	}
	shell := adminshell.New(renderer, registry, "THOR", errors)
	cookies := browserauth.NewCookieManager("session", false, time.Hour)
	authentication := browserauth.NewHTTP(routeAuthentication{browserauth.Principal{UserID: 1, Username: "manager", Permissions: permissions}}, renderer, cookies, "THOR", logger, func(context.Context, audit.Event) error { return nil }, errors)
	router := chi.NewRouter()
	router.Use(authentication.LoadPrincipal)
	NewHandler(shell, NewService(&fakeStore{}, definitions)).RegisterRoutes(router)
	token, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	return router, token
}
