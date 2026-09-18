package slik

import "github.com/ibldzn/trs/internal/access"

// Keep the existing key so all role assignments survive the CSV-to-SLIK transition.
const PermissionGenerate = "reporting.generate"

func PermissionDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{{Key: PermissionGenerate, Name: "Generate SLIK Workbooks", Group: "Reporting", Description: "Allow SLIK XLSX generation"}}
}
