package browserauth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/user"
)

func TestLoginLockoutStopsForwardingToFincloudUntilExpiry(t *testing.T) {
	now := time.Now().UTC()
	authenticator := &fakeAuthenticator{err: fincloud.ErrInvalidCredentials}
	service := testService(t, authenticator, &fakeUsers{found: user.User{ID: 7, Username: "user001", IsActive: true}}, &fakePermissions{}, &fakeSessions{})
	input := LoginInput{Username: " USER001 ", Password: "bad", LocationID: "1", RoleID: "2"}
	for i := 0; i < 5; i++ {
		input.Username = []string{" USER001 ", "user001"}[i%2] // case variants share one counter
		if _, err := service.Login(context.Background(), input, now); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	authenticator.err = nil
	if _, err := service.Login(context.Background(), input, now.Add(time.Minute)); !errors.Is(err, errLoginThrottled) || errors.Is(err, ErrInvalidCredentials) || authenticator.calls != 5 {
		t.Fatalf("locked login err=%v fincloud calls=%d", err, authenticator.calls)
	}
	if _, err := service.Login(context.Background(), input, now.Add(15*time.Minute)); err != nil {
		t.Fatalf("login after lockout expiry: %v", err)
	}
	if len(service.failures.entries) != 0 {
		t.Fatal("successful login did not reset failures")
	}
}

func TestLoginLockoutIgnoresFincloudOutages(t *testing.T) {
	authenticator := &fakeAuthenticator{err: errors.New("upstream unavailable")}
	service := testService(t, authenticator, &fakeUsers{}, &fakePermissions{}, &fakeSessions{})
	for i := 0; i < 10; i++ {
		_, _ = service.Login(context.Background(), LoginInput{Username: "user", Password: "secret", LocationID: "1", RoleID: "2"}, time.Now())
	}
	if len(service.failures.entries) != 0 || authenticator.calls != 10 {
		t.Fatalf("outage counted as failure: entries=%v calls=%d", service.failures.entries, authenticator.calls)
	}
}

func TestLockoutConfigRequiresPositiveBounds(t *testing.T) {
	for _, config := range []LockoutConfig{{}, {Window: time.Minute, MaxFailures: 1}, {Window: time.Minute, Lockout: time.Minute}, {MaxFailures: 1, Lockout: time.Minute}} {
		if _, err := NewService(&fakeAuthenticator{}, &fakeUsers{}, &fakePermissions{}, &fakeSessions{}, time.Hour, time.Hour, nil, config); err == nil {
			t.Fatalf("config %+v accepted", config)
		}
	}
}

func TestLoginThrottleBoundedMemory(t *testing.T) {
	now := time.Now()
	f := loginFailures{entries: make(map[string]loginFailure), window: time.Minute, lockout: 2 * time.Minute, max: 1}
	for i := 0; i < maxLoginFailureEntries+10; i++ {
		f.fail(fmt.Sprint(i), now)
	}
	if len(f.entries) != maxLoginFailureEntries || f.blocked("new", now) {
		t.Fatalf("entries=%d", len(f.entries))
	}
	f.fail("new", now)
	if len(f.entries) != maxLoginFailureEntries {
		t.Fatal("overflow failure grew the map")
	}
	if !f.blocked("0", now.Add(time.Minute)) {
		t.Fatal("window cleanup erased live lockout")
	}
	if f.blocked("new", now.Add(2*time.Minute)) || len(f.entries) != 0 {
		t.Fatal("stale entries not reclaimed")
	}
	f.fail("new", now.Add(2*time.Minute))
	if len(f.entries) != 1 || f.entries["new"].count != 1 || !f.blocked("new", now.Add(2*time.Minute)) {
		t.Fatal("new failures not tracked after stale cleanup")
	}
}
