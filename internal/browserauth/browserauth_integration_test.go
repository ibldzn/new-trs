//go:build integration

package browserauth

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/testutil/integrationdb"
	"github.com/ibldzn/trs/internal/user"
)

type integrationAuthenticator struct{ err error }

func (integrationAuthenticator) Labels(context.Context) (fincloud.AuthLabels, error) {
	return fincloud.AuthLabels{}, nil
}
func (authenticator integrationAuthenticator) Authenticate(context.Context, string, string, string, string) error {
	return authenticator.err
}

func browserDefinitions() []access.PermissionDefinition {
	return []access.PermissionDefinition{
		{Key: access.PermissionLoanInquiry, Name: "Loan Inquiry", Group: "Loans"},
		{Key: access.PermissionManage, Name: "Manage Access", Group: "Access"},
		{Key: "reporting.generate", Name: "Generate Reports", Group: "Reporting"},
		{Key: "lps.generate", Name: "Generate LPS", Group: "Reporting"},
	}
}

func TestFincloudLoginAutoProvisionAndLivePermissions(t *testing.T) {
	database := integrationdb.Open(t)
	integrationdb.Reset(t, database, browserDefinitions())
	service := integrationService(t, database, integrationAuthenticator{})
	ctx := context.Background()
	now := integrationdb.Now()
	input := LoginInput{Username: "  USER001  ", Password: "fincloud-secret", LocationID: "001", RoleID: "R1"}
	first, err := service.Login(ctx, input, now)
	if err != nil || !first.Provisioned {
		t.Fatalf("first login=%+v err=%v", first, err)
	}
	input.Username = "user001"
	second, err := service.Login(ctx, input, now.Add(time.Second))
	if err != nil || second.Provisioned || second.User.ID != first.User.ID {
		t.Fatalf("second login=%+v err=%v", second, err)
	}
	var users, passwordColumns int
	if err := database.GetContext(ctx, &users, `SELECT COUNT(*) FROM users WHERE username='user001'`); err != nil {
		t.Fatal(err)
	}
	if err := database.GetContext(ctx, &passwordColumns, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='users' AND column_name='password_hash'`); err != nil {
		t.Fatal(err)
	}
	if users != 1 || passwordColumns != 0 {
		t.Fatalf("users=%d password_columns=%d", users, passwordColumns)
	}

	principal, err := service.ResolveSession(ctx, auth.HashToken(first.RawToken), now.Add(2*time.Second))
	if err != nil || !principal.Can(access.PermissionLoanInquiry) || principal.Can("reporting.generate") || principal.Can("lps.generate") || principal.Can(access.PermissionManage) {
		t.Fatalf("baseline principal=%+v err=%v", principal, err)
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO user_permissions (user_id, permission_id) SELECT ?, id FROM permissions WHERE `+"`key`"+`='reporting.generate'`, first.User.ID); err != nil {
		t.Fatal(err)
	}
	principal, err = service.ResolveSession(ctx, auth.HashToken(first.RawToken), now.Add(3*time.Second))
	if err != nil || !principal.Can("reporting.generate") {
		t.Fatalf("granted principal=%+v err=%v", principal, err)
	}
	if _, err := database.ExecContext(ctx, `DELETE up FROM user_permissions up JOIN permissions p ON p.id=up.permission_id WHERE up.user_id=? AND p.key='reporting.generate'`, first.User.ID); err != nil {
		t.Fatal(err)
	}
	principal, err = service.ResolveSession(ctx, auth.HashToken(first.RawToken), now.Add(4*time.Second))
	if err != nil || principal.Can("reporting.generate") || !principal.Can(access.PermissionLoanInquiry) {
		t.Fatalf("revoked principal=%+v err=%v", principal, err)
	}
}

func TestConcurrentFirstLoginCreatesOneIdentityAndFailuresCreateNone(t *testing.T) {
	database := integrationdb.Open(t)
	integrationdb.Reset(t, database, browserDefinitions())
	service := integrationService(t, database, integrationAuthenticator{})
	ctx := context.Background()
	var wait sync.WaitGroup
	ids := make(chan uint64, 8)
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.Login(ctx, LoginInput{Username: "RACEUSER", Password: "secret", LocationID: "1", RoleID: "2"}, integrationdb.Now())
			if err != nil {
				ids <- 0
				return
			}
			ids <- result.User.ID
		}()
	}
	wait.Wait()
	close(ids)
	var firstID uint64
	for id := range ids {
		if id == 0 {
			t.Fatal("concurrent login failed")
		}
		if firstID == 0 {
			firstID = id
		} else if id != firstID {
			t.Fatalf("different IDs: %d and %d", firstID, id)
		}
	}
	var count int
	if err := database.GetContext(ctx, &count, `SELECT COUNT(*) FROM users WHERE username='raceuser'`); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}

	failed := integrationService(t, database, integrationAuthenticator{err: fincloud.ErrInvalidCredentials})
	_, err := failed.Login(ctx, LoginInput{Username: "never-created", Password: "wrong", LocationID: "1", RoleID: "2"}, integrationdb.Now())
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("failure=%v", err)
	}
	if err := database.GetContext(ctx, &count, `SELECT COUNT(*) FROM users WHERE username='never-created'`); err != nil || count != 0 {
		t.Fatalf("failed authentication created users=%d err=%v", count, err)
	}
}

func TestInactiveIdentityCannotLoginButReactivationCan(t *testing.T) {
	database := integrationdb.Open(t)
	integrationdb.Reset(t, database, browserDefinitions())
	ctx := context.Background()
	found := integrationdb.User(t, database, "inactive", false)
	service := integrationService(t, database, integrationAuthenticator{})
	input := LoginInput{Username: "inactive", Password: "valid", LocationID: "1", RoleID: "2"}
	if _, err := service.Login(ctx, input, integrationdb.Now()); !errors.Is(err, ErrInactiveUser) {
		t.Fatalf("inactive login=%v", err)
	}
	var sessions int
	if err := database.GetContext(ctx, &sessions, `SELECT COUNT(*) FROM sessions WHERE user_id=?`, found.ID); err != nil || sessions != 0 {
		t.Fatalf("sessions=%d err=%v", sessions, err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE users SET is_active=TRUE WHERE id=?`, found.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(ctx, input, integrationdb.Now()); err != nil {
		t.Fatalf("reactivated login=%v", err)
	}
}

func integrationService(t *testing.T, database *sqlx.DB, authenticator fincloudAuthenticator) *Service {
	t.Helper()
	service, err := NewService(authenticator, user.NewRepository(database), access.NewRepository(database), auth.NewSessionRepository(database), time.Hour, 30*24*time.Hour, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return service
}
