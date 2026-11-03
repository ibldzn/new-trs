package browserauth

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// LockoutConfig bounds failed logins per normalized username so a guessing
// client cannot keep forwarding passwords to Fincloud.
type LockoutConfig struct {
	Window      time.Duration
	MaxFailures int
	Lockout     time.Duration
}

func (config LockoutConfig) validate() error {
	if config.Window <= 0 || config.MaxFailures <= 0 || config.Lockout <= 0 {
		return fmt.Errorf("login lockout bounds must be positive")
	}
	return nil
}

const maxLoginFailureEntries = 10000

// errLoginThrottled is internal: HTTP renders the same invalid-credentials
// response, but a skipped Fincloud check must not count or create an audit event.
var errLoginThrottled = errors.New("login throttled")

type loginFailure struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

// ponytail: in-memory per process; a restart or a second replica resets counts. Move to the DB if TRS runs replicated.
type loginFailures struct {
	mu              sync.Mutex
	entries         map[string]loginFailure
	window, lockout time.Duration
	max             int
	lastCleanup     time.Time
}

func newLoginFailures(config LockoutConfig) loginFailures {
	return loginFailures{entries: make(map[string]loginFailure), window: config.Window, max: config.MaxFailures, lockout: config.Lockout}
}

func (f *loginFailures) cleanup(now time.Time) {
	if now.Sub(f.lastCleanup) < min(time.Minute, f.window, f.lockout) {
		return
	}
	for key, entry := range f.entries {
		if !entry.lockedUntil.IsZero() {
			if !entry.lockedUntil.After(now) {
				delete(f.entries, key)
			}
		} else if !entry.first.Add(f.window).After(now) {
			delete(f.entries, key)
		}
	}
	f.lastCleanup = now
}

func (f *loginFailures) blocked(username string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanup(now)
	entry := f.entries[username]
	if !entry.lockedUntil.IsZero() && !entry.lockedUntil.After(now) {
		delete(f.entries, username)
	}
	return entry.lockedUntil.After(now)
}

func (f *loginFailures) fail(username string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanup(now)
	entry := f.entries[username]
	if entry.lockedUntil.After(now) {
		return
	}
	if entry.first.IsZero() || !entry.first.Add(f.window).After(now) || !entry.lockedUntil.IsZero() {
		entry = loginFailure{first: now}
	}
	if _, exists := f.entries[username]; !exists && len(f.entries) >= maxLoginFailureEntries {
		return
	}
	entry.count++
	if entry.count >= f.max {
		entry.lockedUntil = now.Add(f.lockout)
	}
	f.entries[username] = entry
}

func (f *loginFailures) clear(username string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, username)
}
