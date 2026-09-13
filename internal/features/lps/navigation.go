package lps

import "github.com/ibldzn/trs/internal/platform/navigation"

func Navigation() navigation.Item {
	return navigation.Item{Key: "lps", Label: "LPS", Icon: "file-archive", Path: "/lps", Permission: PermissionGenerate, Match: navigation.MatchPrefix}
}
