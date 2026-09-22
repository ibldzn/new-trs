package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/user"
)

func Bootstrap(ctx context.Context, database *sqlx.DB, definitions []PermissionDefinition, configuredManagers string, now time.Time) error {
	if err := ValidateRegistry(definitions); err != nil {
		return fmt.Errorf("validate permission registry: %w", err)
	}
	transaction, err := database.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin access bootstrap: %w", err)
	}
	defer transaction.Rollback()
	repository := NewRepository(transaction)
	now = now.UTC()
	for _, definition := range definitions {
		if err := repository.syncPermission(ctx, definition, now); err != nil {
			return err
		}
	}
	var permissionID uint64
	if err := transaction.GetContext(ctx, &permissionID, `SELECT id FROM permissions WHERE `+"`key`"+` = ? FOR UPDATE`, PermissionManage); err != nil {
		return fmt.Errorf("lock access management permission: %w", err)
	}
	var managers int
	if err := transaction.GetContext(ctx, &managers, `
		SELECT COUNT(*) FROM users u
		JOIN user_permissions up ON up.user_id = u.id
		WHERE u.is_active = TRUE AND up.permission_id = ?`, permissionID); err != nil {
		return fmt.Errorf("count active access managers: %w", err)
	}
	if managers == 0 {
		usernames, err := bootstrapUsernames(configuredManagers)
		if err != nil {
			return err
		}
		for _, username := range usernames {
			var id uint64
			err := transaction.GetContext(ctx, &id, `SELECT id FROM users WHERE username = ? FOR UPDATE`, username)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				result, insertErr := transaction.ExecContext(ctx, `INSERT INTO users (username, name, is_active, created_at, updated_at) VALUES (?, ?, TRUE, ?, ?)`, username, username, now, now)
				if insertErr != nil {
					return fmt.Errorf("create bootstrap user %q: %w", username, insertErr)
				}
				inserted, insertErr := result.LastInsertId()
				if insertErr != nil {
					return fmt.Errorf("read bootstrap user ID: %w", insertErr)
				}
				id = uint64(inserted)
			case err != nil:
				return fmt.Errorf("find bootstrap user %q: %w", username, err)
			default:
				if _, err := transaction.ExecContext(ctx, `UPDATE users SET is_active = TRUE, updated_at = ? WHERE id = ?`, now, id); err != nil {
					return fmt.Errorf("activate bootstrap user %q: %w", username, err)
				}
			}
			if _, err := transaction.ExecContext(ctx, `INSERT IGNORE INTO user_permissions (user_id, permission_id) VALUES (?, ?)`, id, permissionID); err != nil {
				return fmt.Errorf("grant bootstrap access to %q: %w", username, err)
			}
			if err := audit.Append(ctx, transaction, audit.Event{Attribution: audit.Attribution{SystemActor: "system:access-bootstrap"}, Action: audit.ActionAccessBootstrap, Resource: audit.ResourceUser, ResourceID: id, CreatedAt: now}); err != nil {
				return err
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit access bootstrap: %w", err)
	}
	return nil
}

func bootstrapUsernames(value string) ([]string, error) {
	seen := map[string]struct{}{}
	result := make([]string, 0)
	for _, raw := range strings.Split(value, ",") {
		username := user.NormalizeUsername(raw)
		if username == "" {
			continue
		}
		if err := user.ValidateUsername(username); err != nil {
			return nil, fmt.Errorf("invalid THOR_BOOTSTRAP_ACCESS_MANAGERS username %q: %w", raw, err)
		}
		if _, duplicate := seen[username]; duplicate {
			continue
		}
		seen[username] = struct{}{}
		result = append(result, username)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("THOR_BOOTSTRAP_ACCESS_MANAGERS must contain at least one username while no active access manager exists")
	}
	return result, nil
}
