package auditlogs

import (
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

type fakeStore struct {
	action     string
	limit      int
	offset     int
	countCalls int
	listCalls  int
	records    []Record
	found      Record
	findErr    error
}

func (store *fakeStore) Count(_ context.Context, action string) (int64, error) {
	store.action, store.countCalls = action, store.countCalls+1
	return 101, nil
}

func (store *fakeStore) List(_ context.Context, action string, limit, offset int) ([]Record, error) {
	store.action, store.limit, store.offset, store.listCalls = action, limit, offset, store.listCalls+1
	return store.records, nil
}

func (store *fakeStore) Find(context.Context, uint64) (Record, error) {
	return store.found, store.findErr
}

func TestListUsesTwoQueriesAndFixedPageSize(t *testing.T) {
	store := &fakeStore{}
	page, err := NewService(store).List(context.Background(), "  user.created ", 2)
	if err != nil {
		t.Fatal(err)
	}
	if store.countCalls != 1 || store.listCalls != 1 || store.action != "user.created" || store.limit != 50 || store.offset != 50 {
		t.Fatalf("unexpected calls: %+v", store)
	}
	if page.Pagination.Total != 101 || page.Pagination.TotalPages != 3 {
		t.Fatalf("unexpected pagination: %+v", page.Pagination)
	}
}

func TestIdentityMetadataAndLinks(t *testing.T) {
	actorID, effectiveID := uint64(1), uint64(2)
	record := Record{ActorUserID: &actorID, ActorUsername: "admin", EffectiveUserID: &effectiveID, EffectiveUsername: "member", Metadata: []byte(`{"note":"<script>alert(1)</script>"}`)}
	if record.Actor().Label != "@admin" || record.Effective().Label != "@member" {
		t.Fatalf("unexpected identities: actor=%+v effective=%+v", record.Actor(), record.Effective())
	}
	parsed := template.Must(template.New("metadata").Parse(`{{.MetadataText}}`))
	var output strings.Builder
	if err := parsed.Execute(&output, record); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "<script>") || !strings.Contains(output.String(), "&lt;script&gt;") {
		t.Fatalf("metadata was not escaped: %s", output.String())
	}
	output.Reset()
	if err := parsed.Execute(&output, Record{Metadata: []byte(`<img src=x onerror=alert(1)>`)}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "<img") || !strings.Contains(output.String(), "&lt;img") || (Record{}).MetadataText() != "—" {
		t.Fatalf("invalid/null metadata was unsafe: %s", output.String())
	}
	if got := pageURL("user.created", 2); got != "/audit-logs?action=user.created&page=2" {
		t.Fatalf("filter not preserved: %s", got)
	}
	if label := (Record{Action: string(audit.ActionAuthRegistration)}).Actor().Label; label != "Public" {
		t.Fatalf("registration actor = %q", label)
	}
	if label := (Record{Action: string(audit.ActionAdminBootstrap)}).Actor().Label; label != "System" {
		t.Fatalf("bootstrap actor = %q", label)
	}
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

func TestRoutesRequireAuditPermissionAndRemainReadOnly(t *testing.T) {
	for _, test := range []struct {
		name       string
		principal  browserauth.Principal
		path       string
		method     string
		wantStatus int
	}{
		{name: "permissionless", principal: browserauth.Principal{}, path: "/audit-logs", method: http.MethodGet, wantStatus: http.StatusForbidden},
		{name: "explicit permission", principal: browserauth.Principal{Permissions: access.NewPermissionSet([]string{PermissionView})}, path: "/audit-logs", method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "malformed detail", principal: browserauth.Principal{Permissions: access.NewPermissionSet([]string{PermissionView})}, path: "/audit-logs/nope", method: http.MethodGet, wantStatus: http.StatusNotFound},
		{name: "no mutation", principal: browserauth.Principal{Permissions: access.NewPermissionSet([]string{PermissionView})}, path: "/audit-logs", method: http.MethodPost, wantStatus: http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.principal.UserID, test.principal.Username = 1, "viewer"
			router, token := auditRouter(t, test.principal)
			request := httptest.NewRequest(test.method, test.path, nil)
			request.AddCookie(&http.Cookie{Name: "session", Value: token})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func auditRouter(t *testing.T, principal browserauth.Principal) (http.Handler, string) {
	t.Helper()
	renderer, err := render.New(webfiles.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	errors := render.NewErrorResponder(renderer, "Test", logger)
	registry, err := navigation.NewRegistry([]navigation.Group{{Key: "system", Label: "System", Items: []navigation.Item{Navigation()}}}, PermissionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	shell := adminshell.New(renderer, registry, "Test", errors)
	cookies := browserauth.NewCookieManager("session", false, time.Hour)
	authentication := browserauth.NewHTTP(&fakeAuthentication{principal}, renderer, cookies, "Test", logger, func(context.Context, audit.Event) error { return nil }, errors)
	router := chi.NewRouter()
	router.Use(authentication.LoadPrincipal)
	NewHandler(shell, NewService(&fakeStore{})).RegisterRoutes(router)
	token, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	return router, token
}
