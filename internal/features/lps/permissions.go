package lps

import "github.com/ibldzn/trs/internal/access"

const PermissionGenerate = "lps.generate"

func PermissionDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{{Key: PermissionGenerate, Name: "Generate LPS Reports", Group: "Reporting", Description: "Allow LPS archive generation"}}
}
