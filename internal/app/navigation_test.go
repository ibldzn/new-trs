package app

import (
	"testing"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/features/auditlogs"
	"github.com/ibldzn/trs/internal/features/dashboard"
	"github.com/ibldzn/trs/internal/features/loaninquiry"
	featurelps "github.com/ibldzn/trs/internal/features/lps"
	"github.com/ibldzn/trs/internal/features/roles"
	featureslik "github.com/ibldzn/trs/internal/features/slik"
	"github.com/ibldzn/trs/internal/features/snapshots"
	"github.com/ibldzn/trs/internal/features/users"
	"github.com/ibldzn/trs/internal/platform/navigation"
)

func TestPhaseFourNavigation(t *testing.T) {
	registry, err := navigation.NewRegistry(navigationGroups(), PermissionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		path        string
		permissions []string
		groups      int
		active      string
		open        string
	}{
		{name: "dashboard", path: "/", permissions: []string{dashboard.PermissionView}, groups: 1, active: "dashboard"},
		{name: "users", path: "/users/7", permissions: []string{users.PermissionView}, groups: 1, active: "users"},
		{name: "roles", path: "/roles/7", permissions: []string{roles.PermissionView}, groups: 1, active: "roles", open: "access-control"},
		{name: "audit", path: "/audit-logs/7", permissions: []string{auditlogs.PermissionView}, groups: 1, active: "audit-logs"},
		{name: "loan inquiry", path: "/loans", permissions: []string{loaninquiry.PermissionInquiry}, groups: 1, active: "loan-inquiry"},
		{name: "SLIK", path: "/slik/7", permissions: []string{featureslik.PermissionGenerate}, groups: 1, active: "slik"},
		{name: "LPS", path: "/lps", permissions: []string{featurelps.PermissionGenerate}, groups: 1, active: "lps"},
		{name: "snapshot", path: "/snapshot", permissions: []string{snapshots.PermissionView}, groups: 1, active: "snapshot"},
		{name: "management hidden", path: "/", permissions: nil, groups: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set := access.NewPermissionSet(test.permissions)
			groups := registry.Prepare(test.path, set.Has)
			if len(groups) != test.groups {
				t.Fatalf("expected %d groups, got %#v", test.groups, groups)
			}
			if test.active != "" && !navigationHas(groups, test.active, true, false) {
				t.Fatalf("active item %q missing: %#v", test.active, groups)
			}
			if test.open != "" && !navigationHas(groups, test.open, false, true) {
				t.Fatalf("open item %q missing: %#v", test.open, groups)
			}
		})
	}
}

func TestPermissionAggregation(t *testing.T) {
	definitions := PermissionDefinitions()
	if len(definitions) != 18 {
		t.Fatalf("got %d permissions, want 18", len(definitions))
	}
	if err := access.ValidateRegistry(definitions); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		dashboard.PermissionView: true,
		users.PermissionView:     true, users.PermissionCreate: true, users.PermissionUpdate: true,
		users.PermissionDisable: true, users.PermissionResetPassword: true,
		roles.PermissionView: true, roles.PermissionCreate: true, roles.PermissionUpdate: true,
		roles.PermissionDelete: true, roles.PermissionAssign: true, roles.PermissionManagePermissions: true,
		auditlogs.PermissionView:       true,
		loaninquiry.PermissionInquiry:  true,
		featureslik.PermissionGenerate: true,
		featurelps.PermissionGenerate:  true,
		snapshots.PermissionView:       true, snapshots.PermissionRefresh: true,
	}
	for _, definition := range definitions {
		delete(want, definition.Key)
	}
	if len(want) != 0 {
		t.Fatalf("missing canonical permissions: %v", want)
	}
}

func navigationHas(groups []navigation.GroupView, key string, active, open bool) bool {
	var find func([]navigation.ItemView) bool
	find = func(items []navigation.ItemView) bool {
		for _, item := range items {
			if item.Key == key && (!active || item.Active) && (!open || item.Open) {
				return true
			}
			if find(item.Children) {
				return true
			}
		}
		return false
	}
	for _, group := range groups {
		if find(group.Items) {
			return true
		}
	}
	return false
}
