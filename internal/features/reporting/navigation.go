package reporting

import "github.com/ibldzn/trs/internal/platform/navigation"

func Navigation() navigation.Item {
	return navigation.Item{Key: "reporting", Label: "Bulk Reporting", Icon: "file-spreadsheet", Path: "/reports", Permission: PermissionGenerate, Match: navigation.MatchPrefix}
}
