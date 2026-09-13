package dashboard

import "github.com/ibldzn/trs/internal/access"

const PermissionView = "dashboard.view"

func PermissionDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{{
		Key: PermissionView, Name: "View Dashboard", Group: "Dashboard", Description: "Allow viewing the admin dashboard",
	}}
}
