package snapshots

import "github.com/ibldzn/trs/internal/access"

const (
	PermissionView    = "snapshot.view"
	PermissionRefresh = "snapshot.refresh"
)

func PermissionDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{
		{Key: PermissionView, Name: "View Snapshot", Group: "Snapshot", Description: "Allow viewing current snapshot status"},
		{Key: PermissionRefresh, Name: "Refresh Snapshot", Group: "Snapshot", Description: "Allow manual current snapshot refresh"},
	}
}
