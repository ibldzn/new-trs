package app

import (
	"testing"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/features/loaninquiry"
	featureslik "github.com/ibldzn/trs/internal/features/slik"
	"github.com/ibldzn/trs/internal/features/users"
	"github.com/ibldzn/trs/internal/platform/navigation"
)

func TestPermissionRegistryAndAccessNavigation(t *testing.T) {
	definitions := PermissionDefinitions()
	if err := access.ValidateRegistry(definitions); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{access.PermissionManage: true, access.PermissionLoanInquiry: true, featureslik.PermissionViewAll: true}
	for _, definition := range definitions {
		delete(want, definition.Key)
	}
	if len(want) != 0 {
		t.Fatalf("missing permissions: %v", want)
	}
	registry, err := navigation.NewRegistry(navigationGroups(), definitions)
	if err != nil {
		t.Fatal(err)
	}
	groups := registry.Prepare("/access/1", access.NewPermissionSet([]string{users.PermissionManage}).Has)
	if len(groups) != 1 || groups[0].Items[0].Label != "Access Management" {
		t.Fatalf("navigation=%+v", groups)
	}
	baseline := registry.Prepare("/loans/inquiry", access.NewPermissionSet([]string{loaninquiry.PermissionInquiry}).Has)
	if len(baseline) != 1 {
		t.Fatalf("baseline navigation=%+v", baseline)
	}
}
