package slik

import "github.com/ibldzn/trs/internal/access"

// Keep the existing generation key so migrated per-user grants remain valid.
const (
	PermissionGenerate = "reporting.generate"
	PermissionViewAll  = "reporting.view_all"
)

func PermissionDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{
		{Key: PermissionGenerate, Name: "Generate SLIK Workbooks", Group: "Reporting", Description: "Allow SLIK XLSX generation"},
		{Key: PermissionViewAll, Name: "View All Reporting Jobs", Group: "Reporting", Description: "View SLIK history and output owned by other users."},
	}
}
