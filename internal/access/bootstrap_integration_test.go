//go:build integration

package access_test

import (
	"context"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/testutil/integrationdb"
)

func bootstrapDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{
		{Key: access.PermissionLoanInquiry, Name: "Loan Inquiry", Group: "Loans"},
		{Key: access.PermissionManage, Name: "Manage Access", Group: "Access"},
		{Key: "reporting.generate", Name: "Generate Reports", Group: "Reporting"},
	}
}

func TestBootstrapCreatesManagersOnceThenDatabaseRemainsAuthoritative(t *testing.T) {
	database := integrationdb.Open(t)
	truncateAccessTables(t, database)
	ctx := context.Background()
	if err := access.Bootstrap(ctx, database, bootstrapDefinitions(), " Alice,BOB ", integrationdb.Now()); err != nil {
		t.Fatal(err)
	}
	var managers []string
	if err := database.SelectContext(ctx, &managers, `
		SELECT u.username FROM users u
		JOIN user_permissions up ON up.user_id=u.id
		JOIN permissions p ON p.id=up.permission_id
		WHERE u.is_active=TRUE AND p.key=? ORDER BY u.username`, access.PermissionManage); err != nil {
		t.Fatal(err)
	}
	if len(managers) != 2 || managers[0] != "Alice" || managers[1] != "BOB" {
		t.Fatalf("managers=%v", managers)
	}
	if _, err := database.ExecContext(ctx, `DELETE up FROM user_permissions up JOIN users u ON u.id=up.user_id JOIN permissions p ON p.id=up.permission_id WHERE u.username='Alice' AND p.key=?`, access.PermissionManage); err != nil {
		t.Fatal(err)
	}
	if err := access.Bootstrap(ctx, database, bootstrapDefinitions(), "Alice,Charlie", integrationdb.Now()); err != nil {
		t.Fatal(err)
	}
	var aliceGrant, charlieUsers, sessions, auditEvents int
	if err := database.GetContext(ctx, &aliceGrant, `SELECT COUNT(*) FROM user_permissions up JOIN users u ON u.id=up.user_id JOIN permissions p ON p.id=up.permission_id WHERE u.username='Alice' AND p.key=?`, access.PermissionManage); err != nil {
		t.Fatal(err)
	}
	if err := database.GetContext(ctx, &charlieUsers, `SELECT COUNT(*) FROM users WHERE username='Charlie'`); err != nil {
		t.Fatal(err)
	}
	if err := database.GetContext(ctx, &sessions, `SELECT COUNT(*) FROM sessions`); err != nil {
		t.Fatal(err)
	}
	if err := database.GetContext(ctx, &auditEvents, `SELECT COUNT(*) FROM audit_logs WHERE action='access.bootstrap'`); err != nil {
		t.Fatal(err)
	}
	if aliceGrant != 0 || charlieUsers != 0 || sessions != 0 || auditEvents != 2 {
		t.Fatalf("bootstrap reapplied env: alice_grant=%d charlie=%d sessions=%d audit_events=%d", aliceGrant, charlieUsers, sessions, auditEvents)
	}
}

func truncateAccessTables(t *testing.T, database *sqlx.DB) {
	// Kept local to the disposable integration database; callers are protected by integrationdb.Open.
	ctx := context.Background()
	connection, err := database.Connx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS=0`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"audit_logs", "sessions", "user_permissions", "users", "permissions"} {
		if _, err := connection.ExecContext(ctx, `TRUNCATE TABLE `+table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS=1`); err != nil {
		t.Fatal(err)
	}
}
