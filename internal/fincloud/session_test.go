package fincloud

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionManagerReusesSessionAndPreventsLoginStampede(t *testing.T) {
	var logins atomic.Int32
	release := make(chan struct{})
	manager, err := NewSessionManager(func(context.Context) (string, error) {
		logins.Add(1)
		<-release
		return "session-1", nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 20
	results := make(chan string, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			session, sessionErr := manager.Session(context.Background())
			if sessionErr != nil {
				t.Errorf("session: %v", sessionErr)
			}
			results <- session
		}()
	}
	for logins.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wait.Wait()
	close(results)
	for session := range results {
		if session != "session-1" {
			t.Fatalf("session = %q", session)
		}
	}
	if logins.Load() != 1 {
		t.Fatalf("logins = %d", logins.Load())
	}
	if session, err := manager.Session(context.Background()); err != nil || session != "session-1" || logins.Load() != 1 {
		t.Fatalf("reused session=%q logins=%d error=%v", session, logins.Load(), err)
	}
}

func TestSessionManagerWaitingCallerCanCancel(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	manager, _ := NewSessionManager(func(context.Context) (string, error) {
		close(started)
		<-release
		return "session", nil
	}, nil)
	go func() { _, _ = manager.Session(context.Background()) }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Session(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	close(release)
}

func TestSessionManagerRetriesWhenLoginLeaderIsCanceled(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	manager, _ := NewSessionManager(func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "replacement", nil
	}, nil)
	leaderContext, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error)
	go func() { _, err := manager.Session(leaderContext); leaderDone <- err }()
	<-started
	waiterDone := make(chan struct {
		session string
		err     error
	})
	go func() {
		session, err := manager.Session(context.Background())
		waiterDone <- struct {
			session string
			err     error
		}{session, err}
	}()
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v", err)
	}
	result := <-waiterDone
	if result.err != nil || result.session != "replacement" || calls.Load() != 2 {
		t.Fatalf("session=%q calls=%d error=%v", result.session, calls.Load(), result.err)
	}
}
