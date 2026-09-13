package users

import "github.com/ibldzn/trs/internal/platform/navigation"

func Navigation() navigation.Item {
	return navigation.Item{Key: "users", Label: "Users", Icon: "users", Path: "/users", Permission: PermissionView, Match: navigation.MatchPrefix}
}
