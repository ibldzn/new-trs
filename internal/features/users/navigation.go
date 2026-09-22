package users

import "github.com/ibldzn/trs/internal/platform/navigation"

func Navigation() navigation.Item {
	return navigation.Item{Key: "access-management", Label: "Access Management", Icon: "shield", Path: "/access", Permission: PermissionManage, Match: navigation.MatchPrefix}
}
