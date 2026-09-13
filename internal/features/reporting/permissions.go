package reporting

import "github.com/ibldzn/trs/internal/access"

const PermissionGenerate = "reporting.generate"

func PermissionDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{{Key: PermissionGenerate, Name: "Generate Contractual Reports", Group: "Reporting", Description: "Allow bulk contractual CSV generation"}}
}
