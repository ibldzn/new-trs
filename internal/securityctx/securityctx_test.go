package securityctx

import (
	"testing"

	"github.com/ibldzn/trs/internal/access"
)

func TestRequesterUsesExplicitPermissionsOnly(t *testing.T) {
	requester := Requester{Permissions: access.NewPermissionSet([]string{"access.manage"})}
	if !requester.Can("access.manage") || requester.Can("reporting.generate") {
		t.Fatal("unexpected permissions")
	}
}
