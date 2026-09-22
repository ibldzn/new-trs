package users

import (
	"time"

	"github.com/ibldzn/trs/internal/platform/pagination"
)

const UserPageSize = 20

type UserRecord struct {
	ID          uint64     `db:"id"`
	Username    string     `db:"username"`
	Name        string     `db:"name"`
	IsActive    bool       `db:"is_active"`
	LastLoginAt *time.Time `db:"last_login_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

type UserPage struct {
	Users      []UserRecord
	Query      string
	Pagination pagination.Page
}

type PermissionOption struct {
	Key, Name, Description string
	Selected, Inherited    bool
}

type PermissionGroup struct {
	Name        string
	Permissions []PermissionOption
}

type Detail struct {
	User             UserRecord
	PermissionGroups []PermissionGroup
}
