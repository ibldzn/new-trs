package users

import "errors"

var (
	ErrNotFound           = errors.New("access user not found")
	ErrUnknownPermission  = errors.New("unknown permission")
	ErrBaselinePermission = errors.New("baseline permission cannot be changed")
	ErrLastAccessManager  = errors.New("at least one active access manager must remain")
)
