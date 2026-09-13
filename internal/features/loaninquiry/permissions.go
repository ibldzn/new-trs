package loaninquiry

import "github.com/ibldzn/trs/internal/access"

const PermissionInquiry = "loan.inquiry"

func PermissionDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{{Key: PermissionInquiry, Name: "Loan Inquiry", Group: "Loans", Description: "Allow contractual loan inquiry"}}
}
