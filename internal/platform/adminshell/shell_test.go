package adminshell

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/platform/navigation"
	"github.com/ibldzn/trs/internal/render"
	webfiles "github.com/ibldzn/trs/web"
)

const (
	permissionDashboard = "dashboard.view"
	permissionCustomers = "customers.view"
	permissionReports   = "reports.view"
)

func testPermissions() []access.PermissionDefinition {
	return []access.PermissionDefinition{
		{Key: permissionDashboard, Name: "Dashboard", Group: "General"},
		{Key: permissionCustomers, Name: "Customers", Group: "Operations"},
		{Key: permissionReports, Name: "Reports", Group: "Operations"},
	}
}

func TestRequirePermissionWithoutPrincipalIsInternalError(t *testing.T) {
	renderer, err := render.New(webfiles.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := navigation.NewRegistry(nil, testPermissions())
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(renderer, registry, "Go Admin", render.NewErrorResponder(renderer, "Go Admin", logger))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.RequirePermission(permissionDashboard)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler must not run")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", response.Code)
	}
}

func TestAdminShellRendersFilteredNestedNavigation(t *testing.T) {
	renderer, err := render.New(webfiles.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := navigation.NewRegistry([]navigation.Group{{Key: "operations", Label: "Operations", Items: []navigation.Item{
		{Key: "work", Label: "Work", Children: []navigation.Item{
			{Key: "reports", Label: "Reports", Path: "/reports", Permission: permissionReports, Match: navigation.MatchPrefix},
			{Key: "customers", Label: "Customers", Path: "/customers", Permission: permissionCustomers, Match: navigation.MatchPrefix},
		}},
	}}}, testPermissions())
	if err != nil {
		t.Fatal(err)
	}
	principal := browserauth.Principal{Username: "viewer", Name: "Viewer", Permissions: access.NewPermissionSet([]string{permissionReports})}
	data := PageData{
		Title:       "Reports",
		AppName:     "Go Admin",
		Principal:   principal,
		Navigation:  registry.Prepare("/reports/7", principal.Can),
		CurrentPath: "/reports/7",
	}
	response := httptest.NewRecorder()
	if err := renderer.RenderPage(response, http.StatusOK, "features/dashboard/index", data); err != nil {
		t.Fatal(err)
	}
	body := response.Body.String()
	for _, expected := range []string{"Operations", "Work", "Reports", `aria-current="page"`, `aria-expanded="true"`, `id="nav-children-work"`, `data-navigation-key="work"`, `data-navigation-active="true"`, `data-navigation-manual-open="false"`, `method="post" action="/logout"`} {
		if !strings.Contains(body, expected) {
			t.Errorf("response does not contain %q", expected)
		}
	}
	if strings.Contains(body, ">Customers<") {
		t.Fatal("unauthorized navigation item rendered")
	}
	for _, key := range []string{"sidebar-collapsed", "sidebar-disclosures"} {
		if script, stylesheet := strings.Index(body, `localStorage.getItem("`+key+`")`), strings.Index(body, `rel="stylesheet"`); script < 0 || stylesheet < 0 || script > stylesheet {
			t.Fatalf("persisted %s state is not initialized before stylesheet", key)
		}
	}
	if !strings.Contains(body, `data-sidebar-disclosure-pending="true"`) || !strings.Contains(body, `root.removeAttribute("data-sidebar-disclosure-pending")`) {
		t.Fatal("disclosure navigation is not hidden and revealed around pre-paint initialization")
	}

	data.Navigation = registry.Prepare("/", principal.Can)
	response = httptest.NewRecorder()
	if err := renderer.RenderPage(response, http.StatusOK, "features/dashboard/index", data); err != nil {
		t.Fatal(err)
	}
	inactiveBody := response.Body.String()
	for _, expected := range []string{`data-navigation-active="false"`, `aria-expanded="false"`, `style="display:none"`} {
		if !strings.Contains(inactiveBody, expected) {
			t.Errorf("inactive response does not contain %q", expected)
		}
	}
}

func TestFrontendStatePersistenceKeys(t *testing.T) {
	javascript, err := fs.ReadFile(webfiles.Files, "static/js/app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(javascript)
	if !strings.Contains(source, "sidebar-collapsed") || !strings.Contains(source, "sidebar-disclosures") || !strings.Contains(source, "theme") {
		t.Fatal("expected desktop sidebar, disclosure, and theme persistence")
	}
	if strings.Contains(source, `setItem("mobile`) || strings.Contains(source, `getItem("mobile`) {
		t.Fatal("mobile drawer state must not be persisted")
	}
}
