package securityctx

import "github.com/ibldzn/trs/internal/access"

type Requester struct {
	UserID      uint64
	Username    string
	Permissions access.PermissionSet
}

func (requester Requester) Can(permission string) bool { return requester.Permissions.Has(permission) }
