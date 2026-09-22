//go:build integration

package users

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/securityctx"
	"github.com/ibldzn/trs/internal/testutil/integrationdb"
)

func integrationDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{
		{Key: access.PermissionLoanInquiry, Name: "Loan Inquiry", Group: "Loans"},
		{Key: access.PermissionManage, Name: "Manage Access", Group: "Access"},
		{Key: "reporting.generate", Name: "Generate Reports", Group: "Reporting"},
	}
}

func TestPerUserPermissionsStatusAndLastManagerInvariant(t *testing.T) {
	database := integrationdb.Open(t)
	integrationdb.Reset(t, database, integrationDefinitions())
	ctx := context.Background()
	manager := localUserByUsername(t, database, "integration-manager")
	alice := integrationdb.User(t, database, "alice", true)
	bob := integrationdb.User(t, database, "bob", true)
	carol := integrationdb.User(t, database, "carol", true)
	repository := NewRepository(database, nil)
	requester := securityctx.Requester{UserID: manager.ID, Username: manager.Username, Permissions: access.NewPermissionSet([]string{access.PermissionManage})}
	canonical := []string{access.PermissionManage, "reporting.generate"}

	if err := repository.ReplacePermissions(ctx, requester, alice.ID, canonical, []string{"reporting.generate"}, integrationdb.Now()); err != nil {
		t.Fatal(err)
	}
	aliceKeys, _ := repository.ListPermissionKeys(ctx, alice.ID)
	bobKeys, _ := repository.ListPermissionKeys(ctx, bob.ID)
	if len(aliceKeys) != 1 || aliceKeys[0] != "reporting.generate" || len(bobKeys) != 0 {
		t.Fatalf("alice=%v bob=%v", aliceKeys, bobKeys)
	}

	sessionRepository := auth.NewSessionRepository(database)
	integrationdb.Session(t, sessionRepository, carol.ID, false, "carol-session", integrationdb.Now())
	if err := repository.SetUserActive(ctx, requester, carol.ID, false, integrationdb.Now()); err != nil {
		t.Fatal(err)
	}
	var sessions int
	if err := database.GetContext(ctx, &sessions, `SELECT COUNT(*) FROM sessions WHERE user_id=?`, carol.ID); err != nil || sessions != 0 {
		t.Fatalf("sessions=%d err=%v", sessions, err)
	}
	if err := repository.SetUserActive(ctx, requester, carol.ID, true, integrationdb.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if err := repository.ReplacePermissions(ctx, requester, manager.ID, canonical, nil, integrationdb.Now()); !errors.Is(err, ErrLastAccessManager) {
		t.Fatalf("final manager revoke error=%v", err)
	}
	if err := repository.ReplacePermissions(ctx, requester, bob.ID, canonical, []string{access.PermissionManage}, integrationdb.Now()); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplacePermissions(ctx, requester, manager.ID, canonical, nil, integrationdb.Now()); err != nil {
		t.Fatalf("self revoke with another manager: %v", err)
	}
	if err := repository.SetUserActive(ctx, requester, bob.ID, false, integrationdb.Now()); !errors.Is(err, ErrLastAccessManager) {
		t.Fatalf("final manager deactivate error=%v", err)
	}

	if err := repository.ReplacePermissions(ctx, requester, alice.ID, canonical, []string{access.PermissionManage, "reporting.generate"}, integrationdb.Now()); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsFound := make(chan error, 2)
	for id, selected := range map[uint64][]string{alice.ID: {"reporting.generate"}, bob.ID: nil} {
		wait.Add(1)
		go func(id uint64, selected []string) {
			defer wait.Done()
			errorsFound <- repository.ReplacePermissions(ctx, requester, id, canonical, selected, integrationdb.Now())
		}(id, selected)
	}
	wait.Wait()
	close(errorsFound)
	successes, conflicts := 0, 0
	for err := range errorsFound {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrLastAccessManager):
			conflicts++
		default:
			t.Fatalf("concurrent mutation: %v", err)
		}
	}
	var activeManagers int
	if err := database.GetContext(ctx, &activeManagers, `SELECT COUNT(*) FROM users u JOIN user_permissions up ON up.user_id=u.id JOIN permissions p ON p.id=up.permission_id WHERE u.is_active=TRUE AND p.key=?`, access.PermissionManage); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 || activeManagers != 1 {
		t.Fatalf("successes=%d conflicts=%d active_managers=%d", successes, conflicts, activeManagers)
	}
}

func TestUsernameSearchIsCaseSensitive(t *testing.T) {
	database := integrationdb.Open(t)
	integrationdb.Reset(t, database, integrationDefinitions())
	upper := integrationdb.User(t, database, "User001", true)
	lower := integrationdb.User(t, database, "user001", true)
	if _, err := database.Exec(`UPDATE users SET name = CASE id WHEN ? THEN 'Upper display' WHEN ? THEN 'Lower display' END WHERE id IN (?, ?)`, upper.ID, lower.ID, upper.ID, lower.ID); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(database, nil)

	for _, username := range []string{"User001", "user001"} {
		rows, err := repository.ListUsers(context.Background(), username, 10, 0)
		if err != nil || len(rows) != 1 || rows[0].Username != username {
			t.Fatalf("query=%q rows=%+v err=%v", username, rows, err)
		}
	}
}

func localUserByUsername(t *testing.T, database interface {
	GetContext(context.Context, any, string, ...any) error
}, username string) UserRecord {
	t.Helper()
	var found UserRecord
	if err := database.GetContext(context.Background(), &found, `SELECT `+userColumns+` FROM users WHERE username=?`, username); err != nil {
		t.Fatal(err)
	}
	return found
}
