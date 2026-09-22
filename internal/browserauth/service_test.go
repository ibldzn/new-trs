package browserauth

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/user"
)

type fakeAuthenticator struct {
	username, password, location, role string
	err                                error
}

func (*fakeAuthenticator) Labels(context.Context) (fincloud.AuthLabels, error) {
	return fincloud.AuthLabels{}, nil
}
func (fake *fakeAuthenticator) Authenticate(_ context.Context, username, password, location, role string) error {
	fake.username, fake.password, fake.location, fake.role = username, password, location, role
	return fake.err
}

type fakeUsers struct {
	found     user.User
	created   bool
	err       error
	username  string
	findCalls int
}

func (fake *fakeUsers) FindOrCreateFromAuthenticatedUsername(_ context.Context, username string, _ time.Time) (user.User, bool, error) {
	fake.username, fake.findCalls = username, fake.findCalls+1
	return fake.found, fake.created, fake.err
}
func (fake *fakeUsers) FindByID(context.Context, uint64) (user.User, error) {
	return fake.found, fake.err
}
func (*fakeUsers) UpdateLastLoginAt(context.Context, uint64, time.Time) error { return nil }

type fakePermissions struct{ keys []string }

func (fake *fakePermissions) ListPermissionKeysForUser(context.Context, uint64) ([]string, error) {
	return fake.keys, nil
}

type fakeSessions struct {
	created     auth.CreateSessionParams
	found       auth.Session
	createCalls int
}

func (fake *fakeSessions) Create(_ context.Context, params auth.CreateSessionParams, now time.Time) (auth.Session, error) {
	fake.created, fake.createCalls = params, fake.createCalls+1
	return auth.Session{ID: 9, UserID: params.UserID, RememberMe: params.RememberMe, CreatedAt: now, ExpiresAt: params.ExpiresAt, LastSeenAt: params.LastSeenAt}, nil
}
func (fake *fakeSessions) FindValidByTokenHash(context.Context, [32]byte, time.Time) (auth.Session, error) {
	return fake.found, nil
}
func (*fakeSessions) UpdateLastSeenAt(context.Context, uint64, time.Time) error { return nil }
func (*fakeSessions) Revoke(context.Context, [32]byte) error                    { return nil }

func testService(t *testing.T, authenticator *fakeAuthenticator, users *fakeUsers, permissions *fakePermissions, sessions *fakeSessions) *Service {
	t.Helper()
	service, err := NewService(authenticator, users, permissions, sessions, time.Hour, 24*time.Hour, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestFincloudLoginPreservesCaseForLocalIdentity(t *testing.T) {
	authenticator := &fakeAuthenticator{}
	users := &fakeUsers{found: user.User{ID: 7, Username: "USER001", Name: "User", IsActive: true}, created: true}
	sessions := &fakeSessions{}
	result, err := testService(t, authenticator, users, &fakePermissions{}, sessions).Login(context.Background(), LoginInput{Username: "  USER001  ", Password: "secret", LocationID: "001", RoleID: "R1", RememberMe: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if authenticator.username != "USER001" || authenticator.password != "secret" || authenticator.location != "001" || authenticator.role != "R1" {
		t.Fatalf("Fincloud input changed: %+v", authenticator)
	}
	if users.username != "USER001" || result.User.ID != 7 || !result.Provisioned || sessions.created.UserID != 7 || !sessions.created.RememberMe {
		t.Fatalf("local result=%+v user=%q session=%+v", result, users.username, sessions.created)
	}
}

func TestFailedFincloudLoginDoesNotProvisionOrCreateSession(t *testing.T) {
	users, sessions := &fakeUsers{}, &fakeSessions{}
	_, err := testService(t, &fakeAuthenticator{err: fincloud.ErrInvalidCredentials}, users, &fakePermissions{}, sessions).Login(context.Background(), LoginInput{Username: "user", Password: "bad", LocationID: "1", RoleID: "2"}, time.Now())
	if !errors.Is(err, ErrInvalidCredentials) || users.findCalls != 0 || sessions.createCalls != 0 {
		t.Fatalf("err=%v users=%d sessions=%d", err, users.findCalls, sessions.createCalls)
	}
}

func TestFincloudOutageDoesNotProvisionOrMasqueradeAsBadCredentials(t *testing.T) {
	users, sessions := &fakeUsers{}, &fakeSessions{}
	_, err := testService(t, &fakeAuthenticator{err: errors.New("upstream unavailable")}, users, &fakePermissions{}, sessions).Login(context.Background(), LoginInput{Username: "user", Password: "secret", LocationID: "1", RoleID: "2"}, time.Now())
	if err == nil || errors.Is(err, ErrInvalidCredentials) || users.findCalls != 0 || sessions.createCalls != 0 {
		t.Fatalf("err=%v users=%d sessions=%d", err, users.findCalls, sessions.createCalls)
	}
}

func TestInactiveUserGetsNoSession(t *testing.T) {
	users := &fakeUsers{found: user.User{ID: 7, Username: "user", IsActive: false}}
	sessions := &fakeSessions{}
	_, err := testService(t, &fakeAuthenticator{}, users, &fakePermissions{}, sessions).Login(context.Background(), LoginInput{Username: "user", Password: "ok", LocationID: "1", RoleID: "2"}, time.Now())
	if !errors.Is(err, ErrInactiveUser) || sessions.createCalls != 0 {
		t.Fatalf("err=%v sessions=%d", err, sessions.createCalls)
	}
}

func TestResolveSessionAddsBaselineAndExplicitPermissions(t *testing.T) {
	users := &fakeUsers{found: user.User{ID: 7, Username: "user", Name: "User", IsActive: true}}
	sessions := &fakeSessions{found: auth.Session{ID: 4, UserID: 7, LastSeenAt: time.Now()}}
	principal, err := testService(t, &fakeAuthenticator{}, users, &fakePermissions{keys: []string{"reporting.generate"}}, sessions).ResolveSession(context.Background(), [32]byte{1}, time.Now())
	if err != nil || !principal.Can(access.PermissionLoanInquiry) || !principal.Can("reporting.generate") || principal.Can("access.manage") {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
}

func TestResolveSessionRereadsRevokedPermissions(t *testing.T) {
	users := &fakeUsers{found: user.User{ID: 7, Username: "user", Name: "User", IsActive: true}}
	sessions := &fakeSessions{found: auth.Session{ID: 4, UserID: 7, LastSeenAt: time.Now()}}
	permissions := &fakePermissions{keys: []string{"reporting.generate"}}
	service := testService(t, &fakeAuthenticator{}, users, permissions, sessions)
	first, err := service.ResolveSession(context.Background(), [32]byte{1}, time.Now())
	if err != nil || !first.Can("reporting.generate") {
		t.Fatalf("first principal=%+v err=%v", first, err)
	}
	permissions.keys = nil
	second, err := service.ResolveSession(context.Background(), [32]byte{1}, time.Now())
	if err != nil || second.Can("reporting.generate") || !second.Can(access.PermissionLoanInquiry) {
		t.Fatalf("second principal=%+v err=%v", second, err)
	}
}
