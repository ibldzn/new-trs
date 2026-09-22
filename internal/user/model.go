package user

import "time"

type User struct {
	ID          uint64     `db:"id"`
	Username    string     `db:"username"`
	Name        string     `db:"name"`
	IsActive    bool       `db:"is_active"`
	LastLoginAt *time.Time `db:"last_login_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

type CreateParams struct {
	Username string
	Name     string
	IsActive bool
}
