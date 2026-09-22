-- +goose Up
CREATE TABLE user_permissions (
    user_id BIGINT UNSIGNED NOT NULL,
    permission_id BIGINT UNSIGNED NOT NULL,
    PRIMARY KEY (user_id, permission_id),
    KEY idx_user_permissions_permission_id (permission_id),
    CONSTRAINT fk_user_permissions_user
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE,
    CONSTRAINT fk_user_permissions_permission
        FOREIGN KEY (permission_id) REFERENCES permissions (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT IGNORE INTO user_permissions (user_id, permission_id)
SELECT u.id, rp.permission_id
FROM users u
JOIN role_permissions rp ON rp.role_id = u.role_id;

-- Administrators previously had an implicit allow-all bypass.
INSERT IGNORE INTO user_permissions (user_id, permission_id)
SELECT u.id, p.id
FROM users u
JOIN roles r ON r.id = u.role_id AND r.slug = 'admin'
CROSS JOIN permissions p;

-- Force every browser user through Fincloud after the authorization cutover.
DELETE FROM sessions;

ALTER TABLE sessions
    DROP FOREIGN KEY fk_sessions_impersonated_user,
    DROP KEY idx_sessions_impersonated_user_id,
    DROP COLUMN impersonated_user_id;

ALTER TABLE users
    DROP FOREIGN KEY fk_users_role,
    DROP KEY idx_users_role_id,
    DROP COLUMN role_id,
    DROP COLUMN password_hash;

DROP TABLE role_permissions;
DROP TABLE roles;

DELETE FROM permissions
WHERE `key` IN (
    'users.view', 'users.create', 'users.update', 'users.disable', 'users.reset_password',
    'roles.view', 'roles.create', 'roles.update', 'roles.delete', 'roles.assign', 'roles.manage_permissions'
);

-- This migration intentionally has no Down section. Restoring local password
-- hashes and exact role assignments requires the pre-migration database backup.
