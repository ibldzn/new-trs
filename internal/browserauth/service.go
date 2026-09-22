package browserauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/securityctx"
	"github.com/ibldzn/trs/internal/user"
)

const (
	LastSeenTouchInterval = 5 * time.Minute
	maxPasswordBytes      = 1024
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrInactiveUser       = errors.New("local THOR user is inactive")
	ErrUnauthenticated    = errors.New("unauthenticated")
)

type fincloudAuthenticator interface {
	Labels(context.Context) (fincloud.AuthLabels, error)
	Authenticate(context.Context, string, string, string, string) error
}

type userStore interface {
	FindOrCreateFromAuthenticatedUsername(context.Context, string, time.Time) (user.User, bool, error)
	FindByID(context.Context, uint64) (user.User, error)
	UpdateLastLoginAt(context.Context, uint64, time.Time) error
}

type permissionStore interface {
	ListPermissionKeysForUser(context.Context, uint64) ([]string, error)
}

type sessionStore interface {
	Create(context.Context, auth.CreateSessionParams, time.Time) (auth.Session, error)
	FindValidByTokenHash(context.Context, [32]byte, time.Time) (auth.Session, error)
	UpdateLastSeenAt(context.Context, uint64, time.Time) error
	Revoke(context.Context, [32]byte) error
}

type Service struct {
	authenticator    fincloudAuthenticator
	users            userStore
	permissions      permissionStore
	sessions         sessionStore
	lifetime         time.Duration
	rememberLifetime time.Duration
	generateToken    func() (string, error)
	logger           *slog.Logger
}

type LoginInput struct {
	Username   string
	Password   string
	LocationID string
	RoleID     string
	RememberMe bool
}

type LoginResult struct {
	RawToken    string
	Session     auth.Session
	User        user.User
	Provisioned bool
}

type Principal struct {
	UserID      uint64
	Username    string
	Name        string
	Permissions access.PermissionSet
	SessionID   uint64
	RememberMe  bool
}

func (principal Principal) Can(permission string) bool { return principal.Permissions.Has(permission) }

func (principal Principal) SecurityContext() securityctx.Requester {
	return securityctx.Requester{UserID: principal.UserID, Username: principal.Username, Permissions: principal.Permissions}
}

func auditAttributionFromPrincipal(principal Principal) audit.Attribution {
	identity := audit.Identity{UserID: principal.UserID, Username: principal.Username}
	return audit.Attribution{Actor: &identity, Effective: &identity}
}

func NewService(authenticator fincloudAuthenticator, users userStore, permissions permissionStore, sessions sessionStore, lifetime, rememberLifetime time.Duration, logger *slog.Logger) (*Service, error) {
	if authenticator == nil || users == nil || permissions == nil || sessions == nil {
		return nil, fmt.Errorf("browser authentication dependencies are required")
	}
	if lifetime <= 0 || rememberLifetime <= 0 {
		return nil, fmt.Errorf("session lifetimes must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{authenticator: authenticator, users: users, permissions: permissions, sessions: sessions, lifetime: lifetime, rememberLifetime: rememberLifetime, generateToken: auth.GenerateToken, logger: logger}, nil
}

func (service *Service) Labels(ctx context.Context) (fincloud.AuthLabels, error) {
	return service.authenticator.Labels(ctx)
}

func (service *Service) Login(ctx context.Context, input LoginInput, now time.Time) (LoginResult, error) {
	fincloudUsername := strings.TrimSpace(input.Username)
	if fincloudUsername == "" || input.Password == "" || len(input.Password) > maxPasswordBytes || strings.TrimSpace(input.LocationID) == "" || strings.TrimSpace(input.RoleID) == "" {
		return LoginResult{}, ErrInvalidCredentials
	}
	if err := service.authenticator.Authenticate(ctx, fincloudUsername, input.Password, input.LocationID, input.RoleID); err != nil {
		if errors.Is(err, fincloud.ErrInvalidCredentials) {
			return LoginResult{}, ErrInvalidCredentials
		}
		return LoginResult{}, fmt.Errorf("authenticate with Fincloud: %w", err)
	}
	canonicalUsername := user.NormalizeUsername(fincloudUsername)
	if err := user.ValidateUsername(canonicalUsername); err != nil {
		return LoginResult{}, fmt.Errorf("authenticated Fincloud username is not a valid THOR identity: %w", err)
	}
	found, provisioned, err := service.users.FindOrCreateFromAuthenticatedUsername(ctx, canonicalUsername, now.UTC())
	if err != nil {
		return LoginResult{}, fmt.Errorf("find or provision authenticated user: %w", err)
	}
	if !found.IsActive {
		return LoginResult{}, ErrInactiveUser
	}
	rawToken, err := service.generateToken()
	if err != nil {
		return LoginResult{}, err
	}
	now = now.UTC()
	lifetime := service.lifetime
	if input.RememberMe {
		lifetime = service.rememberLifetime
	}
	session, err := service.sessions.Create(ctx, auth.CreateSessionParams{UserID: found.ID, TokenHash: auth.HashToken(rawToken), RememberMe: input.RememberMe, ExpiresAt: now.Add(lifetime), LastSeenAt: now}, now)
	if err != nil {
		return LoginResult{}, fmt.Errorf("create browser session: %w", err)
	}
	if err := service.users.UpdateLastLoginAt(ctx, found.ID, now); err != nil {
		service.logger.WarnContext(ctx, "update last login", "user_id", found.ID, "error", err)
	}
	return LoginResult{RawToken: rawToken, Session: session, User: found, Provisioned: provisioned}, nil
}

func (service *Service) ResolveSession(ctx context.Context, tokenHash [32]byte, now time.Time) (Principal, error) {
	now = now.UTC()
	session, err := service.sessions.FindValidByTokenHash(ctx, tokenHash, now)
	if errors.Is(err, auth.ErrSessionNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("find browser session: %w", err)
	}
	found, err := service.users.FindByID(ctx, session.UserID)
	if errors.Is(err, user.ErrNotFound) || err == nil && !found.IsActive {
		return Principal{}, service.revokeUnauthenticated(ctx, tokenHash)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("find session user: %w", err)
	}
	keys, err := service.permissions.ListPermissionKeysForUser(ctx, found.ID)
	if err != nil {
		return Principal{}, fmt.Errorf("list session user permissions: %w", err)
	}
	keys = append(keys, access.PermissionLoanInquiry)
	if now.Sub(session.LastSeenAt) >= LastSeenTouchInterval {
		if err := service.sessions.UpdateLastSeenAt(ctx, session.ID, now); err != nil {
			service.logger.WarnContext(ctx, "update session activity", "session_id", session.ID, "error", err)
		}
	}
	return Principal{UserID: found.ID, Username: found.Username, Name: found.Name, Permissions: access.NewPermissionSet(keys), SessionID: session.ID, RememberMe: session.RememberMe}, nil
}

func (service *Service) Logout(ctx context.Context, tokenHash [32]byte) error {
	if err := service.sessions.Revoke(ctx, tokenHash); err != nil {
		return fmt.Errorf("revoke browser session: %w", err)
	}
	return nil
}

func (service *Service) revokeUnauthenticated(ctx context.Context, tokenHash [32]byte) error {
	if err := service.sessions.Revoke(ctx, tokenHash); err != nil {
		return fmt.Errorf("revoke unusable session: %w", err)
	}
	return ErrUnauthenticated
}
