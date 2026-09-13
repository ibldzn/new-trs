package app

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
	"github.com/ibldzn/trs/internal/features/loaninquiry"
	featurelps "github.com/ibldzn/trs/internal/features/lps"
	featurereporting "github.com/ibldzn/trs/internal/features/reporting"
	"github.com/ibldzn/trs/internal/features/snapshots"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	"github.com/ibldzn/trs/internal/platform/navigation"
	"github.com/ibldzn/trs/internal/render"
	"github.com/ibldzn/trs/internal/user"
	webfiles "github.com/ibldzn/trs/web"
)

type businessAuthentication struct{ principal browserauth.Principal }

func (*businessAuthentication) Login(context.Context, browserauth.LoginInput, time.Time) (browserauth.LoginResult, error) {
	return browserauth.LoginResult{}, browserauth.ErrInvalidCredentials
}
func (*businessAuthentication) Register(context.Context, browserauth.RegisterInput, time.Time) (user.User, error) {
	return user.User{}, nil
}
func (authentication *businessAuthentication) ResolveSession(context.Context, [32]byte, time.Time) (browserauth.Principal, error) {
	return authentication.principal, nil
}
func (*businessAuthentication) Logout(context.Context, [32]byte) error { return nil }

func TestBusinessPermissionsAreIndependentAndServerEnforced(t *testing.T) {
	tests := []struct {
		name, path, method, role string
		permissions              []string
		want                     int
	}{
		{"loan denied", "/loan", http.MethodGet, access.UserRoleSlug, nil, http.StatusForbidden},
		{"loan allowed for any username", "/loan", http.MethodGet, access.UserRoleSlug, []string{loaninquiry.PermissionInquiry}, http.StatusNoContent},
		{"reporting does not grant LPS", "/lps", http.MethodGet, access.UserRoleSlug, []string{featurereporting.PermissionGenerate}, http.StatusForbidden},
		{"LPS does not grant reporting", "/report", http.MethodGet, access.UserRoleSlug, []string{featurelps.PermissionGenerate}, http.StatusForbidden},
		{"snapshot view does not grant refresh", "/snapshot/refresh", http.MethodPost, access.UserRoleSlug, []string{snapshots.PermissionView}, http.StatusForbidden},
		{"snapshot refresh does not grant view", "/snapshot", http.MethodGet, access.UserRoleSlug, []string{snapshots.PermissionRefresh}, http.StatusForbidden},
		{"admin bypass", "/snapshot/refresh", http.MethodPost, access.AdminRoleSlug, nil, http.StatusNoContent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router, token := businessRBACRouter(t, browserauth.Principal{
				UserID: 7, Username: "not-an-allowlisted-name", RoleID: 2, RoleSlug: test.role,
				Actor:       browserauth.Identity{UserID: 7, Username: "not-an-allowlisted-name", RoleID: 2, RoleSlug: test.role},
				Permissions: access.NewPermissionSet(test.permissions),
			})
			request := httptest.NewRequest(test.method, test.path, nil)
			request.AddCookie(&http.Cookie{Name: "session", Value: token})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestUpstreamFailureDoesNotClearLocalSession(t *testing.T) {
	router, token := businessRBACRouter(t, browserauth.Principal{
		UserID: 7, Username: "member", RoleID: 2, RoleSlug: access.UserRoleSlug,
		Actor:       browserauth.Identity{UserID: 7, Username: "member", RoleID: 2, RoleSlug: access.UserRoleSlug},
		Permissions: access.NewPermissionSet([]string{loaninquiry.PermissionInquiry}),
	})
	request := httptest.NewRequest(http.MethodGet, "/fincloud-failure", nil)
	request.AddCookie(&http.Cookie{Name: "session", Value: token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("status=%d cookie=%q", response.Code, response.Header().Get("Set-Cookie"))
	}
	request = httptest.NewRequest(http.MethodGet, "/loan", nil)
	request.AddCookie(&http.Cookie{Name: "session", Value: token})
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("local session invalidated: status=%d", response.Code)
	}
}

func businessRBACRouter(t *testing.T, principal browserauth.Principal) (http.Handler, string) {
	t.Helper()
	renderer, err := render.New(webfiles.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	errors := render.NewErrorResponder(renderer, "Test", logger)
	registry, err := navigation.NewRegistry(nil, PermissionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	admin := adminshell.New(renderer, registry, "Test", errors)
	cookies := browserauth.NewCookieManager("session", false, time.Hour)
	authentication := browserauth.NewHTTP(&businessAuthentication{principal: principal}, renderer, cookies, "Test", false, logger, func(context.Context, audit.Event) error { return nil }, errors)
	router := chi.NewRouter()
	router.Use(authentication.LoadPrincipal)
	router.Group(func(protected chi.Router) {
		protected.Use(authentication.RequireAuth)
		protected.With(admin.RequirePermission(loaninquiry.PermissionInquiry)).Get("/loan", noContent)
		protected.With(admin.RequirePermission(loaninquiry.PermissionInquiry)).Get("/fincloud-failure", func(writer http.ResponseWriter, _ *http.Request) {
			http.Error(writer, "upstream unavailable", http.StatusServiceUnavailable)
		})
		protected.With(admin.RequirePermission(featurereporting.PermissionGenerate)).Get("/report", noContent)
		protected.With(admin.RequirePermission(featurelps.PermissionGenerate)).Get("/lps", noContent)
		protected.With(admin.RequirePermission(snapshots.PermissionView)).Get("/snapshot", noContent)
		protected.With(admin.RequirePermission(snapshots.PermissionRefresh)).Post("/snapshot/refresh", noContent)
	})
	token, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	return router, token
}

func noContent(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) }
