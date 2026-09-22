package access

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

type Repository struct{ database sqlx.ExtContext }

func NewRepository(database sqlx.ExtContext) *Repository { return &Repository{database: database} }

func (repository *Repository) ListPermissionKeysForUser(ctx context.Context, userID uint64) ([]string, error) {
	const query = `
		SELECT p.key FROM user_permissions up
		JOIN permissions p ON p.id = up.permission_id
		WHERE up.user_id = ? ORDER BY p.key`
	keys := make([]string, 0)
	if err := sqlx.SelectContext(ctx, repository.database, &keys, query, userID); err != nil {
		return nil, fmt.Errorf("list permissions for user %d: %w", userID, err)
	}
	return keys, nil
}

func (repository *Repository) UserHasPermission(ctx context.Context, userID uint64, key string) (bool, error) {
	var allowed bool
	if err := sqlx.GetContext(ctx, repository.database, &allowed, `
		SELECT EXISTS (
			SELECT 1 FROM user_permissions up
			JOIN permissions p ON p.id = up.permission_id
			WHERE up.user_id = ? AND p.key = ?
		)`, userID, key); err != nil {
		return false, fmt.Errorf("check user permission: %w", err)
	}
	return allowed, nil
}

func (repository *Repository) syncPermission(ctx context.Context, definition PermissionDefinition, now time.Time) error {
	const query = `
		INSERT INTO permissions (` + "`key`" + `, name, group_name, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			updated_at = IF(
				name COLLATE utf8mb4_bin <> VALUES(name) COLLATE utf8mb4_bin OR
				group_name COLLATE utf8mb4_bin <> VALUES(group_name) COLLATE utf8mb4_bin OR
				description COLLATE utf8mb4_bin <> VALUES(description) COLLATE utf8mb4_bin,
				VALUES(updated_at), updated_at),
			name = VALUES(name), group_name = VALUES(group_name), description = VALUES(description)`
	if _, err := repository.database.ExecContext(ctx, query, definition.Key, definition.Name, definition.Group, definition.Description, now, now); err != nil {
		return fmt.Errorf("sync permission %q: %w", definition.Key, err)
	}
	return nil
}
