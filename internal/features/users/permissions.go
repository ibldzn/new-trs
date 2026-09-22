package users

import "github.com/ibldzn/trs/internal/access"

const PermissionManage = access.PermissionManage

func PermissionDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{{Key: PermissionManage, Name: "Manage Access", Group: "Access", Description: "Manage THOR user activation and per-user permissions."}}
}
