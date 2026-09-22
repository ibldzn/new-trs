//go:build integration

package access_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"

	"github.com/ibldzn/trs/internal/testutil/integrationdb"
)

func TestFincloudAuthorizationMigrationPreservesIdentityAndEffectiveAccess(t *testing.T) {
	database := integrationdb.Open(t)
	ctx := context.Background()
	migrations := filepath.Join(integrationdb.Root(t), "migrations")
	if err := rebuildSchema(ctx, database, migrations, 202609210001); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rebuildSchema(context.Background(), database, migrations, 0); err != nil {
			t.Errorf("restore latest schema: %v", err)
		}
	})
	seed := []string{
		`INSERT INTO roles (id,name,slug,is_system,created_at,updated_at) VALUES (1,'Administrator','admin',TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),(2,'User','user',TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO permissions (id,` + "`key`" + `,name,group_name,description,created_at,updated_at) VALUES (10,'dashboard.view','Dashboard','General','',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),(11,'reporting.generate','Reporting','Reporting','',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),(12,'users.view','Users','Access','',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO role_permissions (role_id,permission_id) VALUES (2,11)`,
		`INSERT INTO users (id,username,name,password_hash,role_id,is_active,created_at,updated_at) VALUES (42,'admin','Admin','hash',1,TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),(43,'branch','Branch','hash',2,TRUE,UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO sessions (user_id,token_hash,remember_me,expires_at,last_seen_at,created_at,updated_at) VALUES (42,UNHEX(REPEAT('01',32)),FALSE,DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 DAY),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
	}
	for _, statement := range seed {
		if _, err := database.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := goose.UpContext(ctx, database.DB, migrations); err != nil {
		t.Fatal(err)
	}

	var ids []uint64
	if err := database.SelectContext(ctx, &ids, `SELECT id FROM users ORDER BY id`); err != nil {
		t.Fatal(err)
	}
	var sessions, removedColumns, removedTables, obsoletePermissions int
	checks := []struct {
		destination *int
		query       string
	}{
		{&sessions, `SELECT COUNT(*) FROM sessions`},
		{&removedColumns, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='users' AND column_name IN ('password_hash','role_id')`},
		{&removedTables, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name IN ('roles','role_permissions')`},
		{&obsoletePermissions, `SELECT COUNT(*) FROM permissions WHERE ` + "`key`" + `='users.view'`},
	}
	for _, check := range checks {
		if err := database.GetContext(ctx, check.destination, check.query); err != nil {
			t.Fatal(err)
		}
	}
	var grants []struct {
		Username string `db:"username"`
		Key      string `db:"key"`
	}
	if err := database.SelectContext(ctx, &grants, `SELECT u.username,p.key FROM user_permissions up JOIN users u ON u.id=up.user_id JOIN permissions p ON p.id=up.permission_id ORDER BY u.username,p.key`); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids) != "[42 43]" || sessions != 0 || removedColumns != 0 || removedTables != 0 || obsoletePermissions != 0 || fmt.Sprint(grants) != "[{admin dashboard.view} {admin reporting.generate} {branch reporting.generate}]" {
		t.Fatalf("ids=%v sessions=%d columns=%d tables=%d obsolete=%d grants=%v", ids, sessions, removedColumns, removedTables, obsoletePermissions, grants)
	}
}

func rebuildSchema(ctx context.Context, database *sqlx.DB, migrations string, target int64) error {
	connection, err := database.Connx(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS=0`); err != nil {
		return err
	}
	var tables []string
	if err := connection.SelectContext(ctx, &tables, `SHOW TABLES`); err != nil {
		return err
	}
	for _, table := range tables {
		identifier := "`" + strings.ReplaceAll(table, "`", "``") + "`"
		if _, err := connection.ExecContext(ctx, `DROP TABLE `+identifier); err != nil {
			return err
		}
	}
	if _, err := connection.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS=1`); err != nil {
		return err
	}
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	if target == 0 {
		return goose.UpContext(ctx, database.DB, migrations)
	}
	return goose.UpToContext(ctx, database.DB, migrations, target)
}
