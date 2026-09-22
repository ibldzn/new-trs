package loaninquiry

import "github.com/ibldzn/trs/internal/platform/navigation"

func Navigation() navigation.Item {
	return navigation.Item{Key: "loan-inquiry", Label: "Loan Inquiry", Icon: "landmark", Path: "/loans/inquiry", Permission: PermissionInquiry, Match: navigation.MatchPrefix}
}
