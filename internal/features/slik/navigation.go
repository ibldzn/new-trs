package slik

import "github.com/ibldzn/trs/internal/platform/navigation"

func Navigation() navigation.Item {
	return navigation.Item{Key: "slik", Label: "SLIK Generator", Icon: "file-spreadsheet", Path: "/slik", Permission: PermissionGenerate, Match: navigation.MatchPrefix}
}
