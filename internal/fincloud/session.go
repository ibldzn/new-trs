package fincloud

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type loginFunc func(context.Context) (string, error)
type logoutFunc func(context.Context, string) error

type SessionManager struct {
	mu       sync.Mutex
	session  string
	retired  string
	login    loginFunc
	logout   logoutFunc
	loginEnd chan struct{}
	loginErr error
}

func NewSessionManager(login loginFunc, logout logoutFunc) (*SessionManager, error) {
	if login == nil {
		return nil, fmt.Errorf("Fincloud login function is required")
	}
	return &SessionManager{login: login, logout: logout}, nil
}

func (manager *SessionManager) Session(ctx context.Context) (string, error) {
	for {
		manager.mu.Lock()
		if manager.session != "" {
			session := manager.session
			manager.mu.Unlock()
			return session, nil
		}
		if manager.loginEnd != nil {
			done := manager.loginEnd
			manager.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-done:
			}
			manager.mu.Lock()
			err := manager.loginErr
			manager.mu.Unlock()
			if err != nil {
				if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() == nil {
					continue
				}
				return "", err
			}
			continue
		}

		done := make(chan struct{})
		manager.loginEnd = done
		manager.loginErr = nil
		manager.mu.Unlock()

		session, err := manager.login(ctx)
		if err == nil && session == "" {
			err = fmt.Errorf("Fincloud login returned empty session")
		}

		manager.mu.Lock()
		retired := ""
		if err == nil {
			manager.session = session
			retired = manager.retired
			manager.retired = ""
		}
		manager.loginErr = err
		manager.loginEnd = nil
		close(done)
		manager.mu.Unlock()

		if err != nil {
			return "", err
		}
		if retired != "" && retired != session && manager.logout != nil {
			cleanupContext, cancel := context.WithTimeout(ctx, 2*time.Second)
			_ = manager.logout(cleanupContext, retired)
			cancel()
		}
		return session, nil
	}
}

func (manager *SessionManager) Invalidate(session string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if session != "" && manager.session == session {
		manager.retired = session
		manager.session = ""
	}
}

func (manager *SessionManager) Close(ctx context.Context) error {
	manager.mu.Lock()
	session := manager.session
	manager.session = ""
	manager.mu.Unlock()
	if session == "" || manager.logout == nil {
		return nil
	}
	return manager.logout(ctx, session)
}
