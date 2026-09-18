package app

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/features/auditlogs"
	"github.com/ibldzn/trs/internal/features/dashboard"
	"github.com/ibldzn/trs/internal/features/impersonation"
	"github.com/ibldzn/trs/internal/features/loaninquiry"
	featurelps "github.com/ibldzn/trs/internal/features/lps"
	"github.com/ibldzn/trs/internal/features/roles"
	featureslik "github.com/ibldzn/trs/internal/features/slik"
	"github.com/ibldzn/trs/internal/features/snapshots"
	"github.com/ibldzn/trs/internal/features/users"
	"github.com/ibldzn/trs/internal/loan"
	corelps "github.com/ibldzn/trs/internal/lps"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	"github.com/ibldzn/trs/internal/platform/navigation"
	"github.com/ibldzn/trs/internal/slik"
	"github.com/ibldzn/trs/internal/snapshot"
	"github.com/ibldzn/trs/internal/user"
)

func PermissionDefinitions() []access.PermissionDefinition {
	definitions := make([]access.PermissionDefinition, 0, 18)
	definitions = append(definitions, dashboard.PermissionDefinitions()...)
	definitions = append(definitions, users.PermissionDefinitions()...)
	definitions = append(definitions, roles.PermissionDefinitions()...)
	definitions = append(definitions, auditlogs.PermissionDefinitions()...)
	definitions = append(definitions, loaninquiry.PermissionDefinitions()...)
	definitions = append(definitions, featureslik.PermissionDefinitions()...)
	definitions = append(definitions, featurelps.PermissionDefinitions()...)
	definitions = append(definitions, snapshots.PermissionDefinitions()...)
	return definitions
}

type featureDependencies struct {
	database  *sqlx.DB
	users     *user.Repository
	access    *access.Repository
	admin     *adminshell.Shell
	cookies   browserauth.CookieManager
	positions interface {
		GetLoanPosition(context.Context, string, loan.Date) (loan.ResolvedPosition, error)
	}
	snapshot interface {
		Status(context.Context) (snapshot.Status, error)
		Refresh(context.Context, snapshot.Trigger, audit.Attribution) (int, error)
	}
	slik *slik.Manager
	lps  interface {
		Generate(context.Context, corelps.Input, io.Writer) (corelps.Result, error)
	}
	location       *time.Location
	maxSLIKUpload  int64
	lpsDefaultCode string
	appendAudit    func(context.Context, audit.Event) error
	logger         *slog.Logger
}

func registerFeatureRoutes(router chi.Router, dependencies featureDependencies) {
	userService := users.NewService(users.NewRepository(dependencies.database, audit.Append), dependencies.access, roles.PermissionAssign)
	roleService := roles.NewService(roles.NewRepository(dependencies.database, audit.Append), PermissionDefinitions())
	impersonationService := impersonation.NewService(
		dependencies.users,
		dependencies.access,
		impersonation.NewRepository(dependencies.database, audit.Append),
	)
	auditLogService := auditlogs.NewService(auditlogs.NewRepository(dependencies.database))

	dashboard.NewHandler(dependencies.admin).RegisterRoutes(router)
	users.NewHandler(dependencies.admin, userService, dependencies.cookies, roles.PermissionAssign, impersonation.CanStart).RegisterRoutes(router)
	roles.NewHandler(dependencies.admin, roleService).RegisterRoutes(router)
	impersonation.NewHandler(dependencies.admin, impersonationService, dependencies.cookies).RegisterRoutes(router)
	auditlogs.NewHandler(dependencies.admin, auditLogService).RegisterRoutes(router)
	loaninquiry.NewHandler(dependencies.admin, dependencies.positions, dependencies.location, dependencies.appendAudit, dependencies.logger).RegisterRoutes(router)
	featureslik.NewHandler(dependencies.admin, dependencies.slik, dependencies.location, dependencies.maxSLIKUpload).RegisterRoutes(router)
	featurelps.NewHandler(dependencies.admin, dependencies.lps, dependencies.lpsDefaultCode, dependencies.location, dependencies.appendAudit, dependencies.logger).RegisterRoutes(router)
	snapshots.NewHandler(dependencies.admin, dependencies.snapshot).RegisterRoutes(router)
}

func navigationGroups() []navigation.Group {
	return []navigation.Group{
		{Key: "general", Label: "General", Items: []navigation.Item{dashboard.Navigation(), loaninquiry.Navigation()}},
		{Key: "reporting", Label: "Reporting", Items: []navigation.Item{featureslik.Navigation(), featurelps.Navigation()}},
		{Key: "management", Label: "Management", Items: []navigation.Item{
			users.Navigation(),
			{Key: "access-control", Label: "Access Control", Icon: "shield", Children: []navigation.Item{roles.Navigation()}},
		}},
		{Key: "system", Label: "System", Items: []navigation.Item{snapshots.Navigation(), auditlogs.Navigation()}},
	}
}
