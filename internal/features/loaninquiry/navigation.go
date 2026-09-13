package loaninquiry

import "github.com/ibldzn/trs/internal/platform/navigation"

func Navigation() navigation.Item {
	return navigation.Item{Key: "loan-inquiry", Label: "Loan Inquiry", Icon: "landmark", Path: "/loans", Permission: PermissionInquiry, Match: navigation.MatchPrefix}
}
