package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/securityctx"
	"github.com/ibldzn/trs/internal/user"
)

const userColumns = `id, username, name, is_active, last_login_at, created_at, updated_at`

type Repository struct {
	database    *sqlx.DB
	appendAudit audit.AppendFunc
	beginTx     func(context.Context, *sql.TxOptions) (*sqlx.Tx, error)
}

func NewRepository(database *sqlx.DB, appendAudit audit.AppendFunc) *Repository {
	if appendAudit == nil {
		appendAudit = audit.Append
	}
	return &Repository{database: database, appendAudit: appendAudit, beginTx: database.BeginTxx}
}

func (repository *Repository) CountUsers(ctx context.Context, query string) (int64, error) {
	statement := `SELECT COUNT(*) FROM users`
	arguments := []any(nil)
	if query != "" {
		username, name := searchPatterns(query)
		statement += ` WHERE username LIKE ? ESCAPE '!' OR name LIKE ? ESCAPE '!'`
		arguments = []any{username, name}
	}
	var total int64
	if err := repository.database.GetContext(ctx, &total, statement, arguments...); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return total, nil
}

func (repository *Repository) ListUsers(ctx context.Context, query string, limit, offset int) ([]UserRecord, error) {
	statement := `SELECT ` + userColumns + ` FROM users`
	arguments := []any(nil)
	if query != "" {
		username, name := searchPatterns(query)
		statement += ` WHERE username LIKE ? ESCAPE '!' OR name LIKE ? ESCAPE '!'`
		arguments = append(arguments, username, name)
	}
	statement += ` ORDER BY username, id LIMIT ? OFFSET ?`
	arguments = append(arguments, limit, offset)
	rows := make([]UserRecord, 0)
	if err := repository.database.SelectContext(ctx, &rows, statement, arguments...); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return rows, nil
}

func (repository *Repository) FindUserByID(ctx context.Context, id uint64) (UserRecord, error) {
	return findUser(ctx, repository.database, id, false)
}

func (repository *Repository) ListPermissionKeys(ctx context.Context, userID uint64) ([]string, error) {
	keys := make([]string, 0)
	if err := repository.database.SelectContext(ctx, &keys, `
		SELECT p.key FROM user_permissions up
		JOIN permissions p ON p.id = up.permission_id
		WHERE up.user_id = ? ORDER BY p.key`, userID); err != nil {
		return nil, fmt.Errorf("list user permissions: %w", err)
	}
	return keys, nil
}

func (repository *Repository) ReplacePermissions(ctx context.Context, requester securityctx.Requester, userID uint64, canonical, selected []string, now time.Time) error {
	transaction, err := repository.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin user permission replacement: %w", err)
	}
	defer transaction.Rollback()
	manageID, err := lockManagePermission(ctx, transaction)
	if err != nil {
		return err
	}
	target, err := findUser(ctx, transaction, userID, true)
	if err != nil {
		return err
	}
	ids, err := permissionIDs(ctx, transaction, canonical)
	if err != nil {
		return err
	}
	if len(ids) != len(canonical) {
		return ErrUnknownPermission
	}
	existing, err := assignedPermissions(ctx, transaction, userID, canonical)
	if err != nil {
		return err
	}
	canonicalIDs := make([]uint64, 0, len(canonical))
	for _, key := range canonical {
		canonicalIDs = append(canonicalIDs, ids[key])
	}
	deleteQuery, deleteArguments, err := sqlx.In(`DELETE FROM user_permissions WHERE user_id = ? AND permission_id IN (?)`, userID, canonicalIDs)
	if err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, transaction.Rebind(deleteQuery), deleteArguments...); err != nil {
		return fmt.Errorf("delete user permissions: %w", err)
	}
	for _, key := range selected {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO user_permissions (user_id, permission_id) VALUES (?, ?)`, userID, ids[key]); err != nil {
			return fmt.Errorf("grant user permission %q: %w", key, err)
		}
	}
	if target.IsActive {
		if err := requireActiveManager(ctx, transaction, manageID); err != nil {
			return err
		}
	}
	added, removed := permissionChanges(existing, selected)
	if err := repository.appendAudit(ctx, transaction, audit.Event{Attribution: attribution(requester), Action: audit.ActionUserPermissionsUpdated, Resource: audit.ResourceUser, ResourceID: userID, Metadata: audit.PermissionsUpdatedMetadata{Added: added, Removed: removed}, CreatedAt: now}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit user permission replacement: %w", err)
	}
	return nil
}

func (repository *Repository) SetUserActive(ctx context.Context, requester securityctx.Requester, userID uint64, active bool, now time.Time) error {
	transaction, err := repository.beginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin user status update: %w", err)
	}
	defer transaction.Rollback()
	manageID, err := lockManagePermission(ctx, transaction)
	if err != nil {
		return err
	}
	target, err := findUser(ctx, transaction, userID, true)
	if err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE users SET is_active = ?, updated_at = ? WHERE id = ?`, active, now, userID); err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	if !active {
		if _, err := transaction.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("revoke deactivated user sessions: %w", err)
		}
		if err := requireActiveManager(ctx, transaction, manageID); err != nil {
			return err
		}
	}
	action := audit.ActionUserDeactivated
	if active {
		action = audit.ActionUserActivated
	}
	if err := repository.appendAudit(ctx, transaction, audit.Event{Attribution: attribution(requester), Action: action, Resource: audit.ResourceUser, ResourceID: userID, Metadata: audit.StatusChangeMetadata{From: status(target.IsActive), To: status(active)}, CreatedAt: now}); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit user status update: %w", err)
	}
	return nil
}

func findUser(ctx context.Context, database sqlx.ExtContext, id uint64, lock bool) (UserRecord, error) {
	query := `SELECT ` + userColumns + ` FROM users WHERE id = ?`
	if lock {
		query += ` FOR UPDATE`
	}
	var found UserRecord
	if err := sqlx.GetContext(ctx, database, &found, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserRecord{}, ErrNotFound
		}
		return UserRecord{}, fmt.Errorf("find user: %w", err)
	}
	return found, nil
}

func lockManagePermission(ctx context.Context, transaction *sqlx.Tx) (uint64, error) {
	var id uint64
	if err := transaction.GetContext(ctx, &id, `SELECT id FROM permissions WHERE `+"`key`"+` = ? FOR UPDATE`, access.PermissionManage); err != nil {
		return 0, fmt.Errorf("lock access management permission: %w", err)
	}
	return id, nil
}

func requireActiveManager(ctx context.Context, transaction *sqlx.Tx, permissionID uint64) error {
	var count int
	if err := transaction.GetContext(ctx, &count, `SELECT COUNT(*) FROM users u JOIN user_permissions up ON up.user_id = u.id WHERE u.is_active = TRUE AND up.permission_id = ?`, permissionID); err != nil {
		return fmt.Errorf("count active access managers: %w", err)
	}
	if count == 0 {
		return ErrLastAccessManager
	}
	return nil
}

func permissionIDs(ctx context.Context, transaction *sqlx.Tx, keys []string) (map[string]uint64, error) {
	query, arguments, err := sqlx.In(`SELECT id, `+"`key`"+` FROM permissions WHERE `+"`key`"+` IN (?) FOR UPDATE`, keys)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID  uint64 `db:"id"`
		Key string `db:"key"`
	}
	if err := transaction.SelectContext(ctx, &rows, transaction.Rebind(query), arguments...); err != nil {
		return nil, fmt.Errorf("resolve permission IDs: %w", err)
	}
	result := make(map[string]uint64, len(rows))
	for _, row := range rows {
		result[row.Key] = row.ID
	}
	return result, nil
}

func assignedPermissions(ctx context.Context, transaction *sqlx.Tx, userID uint64, keys []string) ([]string, error) {
	query, arguments, err := sqlx.In(`SELECT p.key FROM user_permissions up JOIN permissions p ON p.id = up.permission_id WHERE up.user_id = ? AND p.key IN (?)`, userID, keys)
	if err != nil {
		return nil, err
	}
	var result []string
	if err := transaction.SelectContext(ctx, &result, transaction.Rebind(query), arguments...); err != nil {
		return nil, fmt.Errorf("list assigned permissions: %w", err)
	}
	return result, nil
}

func permissionChanges(existing, selected []string) ([]string, []string) {
	old, next := map[string]bool{}, map[string]bool{}
	for _, key := range existing {
		old[key] = true
	}
	for _, key := range selected {
		next[key] = true
	}
	var added, removed []string
	for key := range next {
		if !old[key] {
			added = append(added, key)
		}
	}
	for key := range old {
		if !next[key] {
			removed = append(removed, key)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}

func searchPatterns(query string) (string, string) {
	return "%" + escapeLike(user.NormalizeUsername(query)) + "%", "%" + escapeLike(strings.TrimSpace(query)) + "%"
}

func escapeLike(value string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(value)
}

func attribution(requester securityctx.Requester) audit.Attribution {
	identity := audit.Identity{UserID: requester.UserID, Username: requester.Username}
	return audit.Attribution{Actor: &identity, Effective: &identity}
}

func status(active bool) string {
	if active {
		return "active"
	}
	return "inactive"
}
