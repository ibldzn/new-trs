package snapshots

import "github.com/ibldzn/trs/internal/platform/navigation"

func Navigation() navigation.Item {
	return navigation.Item{Key: "snapshot", Label: "Current Snapshot", Icon: "database-zap", Path: "/snapshot", Permission: PermissionView, Match: navigation.MatchPrefix}
}
