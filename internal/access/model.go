package access

const (
	PermissionLoanInquiry = "loan.inquiry"
	PermissionManage      = "access.manage"
)

type PermissionDefinition struct {
	Key         string
	Name        string
	Group       string
	Description string
}
