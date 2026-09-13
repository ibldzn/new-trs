package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/config"
	"github.com/ibldzn/trs/internal/contractual"
	"github.com/ibldzn/trs/internal/database"
	"github.com/ibldzn/trs/internal/dwh"
	"github.com/ibldzn/trs/internal/fincloud"
	corelps "github.com/ibldzn/trs/internal/lps"
	"github.com/ibldzn/trs/internal/mso"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	"github.com/ibldzn/trs/internal/platform/navigation"
	"github.com/ibldzn/trs/internal/position"
	"github.com/ibldzn/trs/internal/render"
	corereporting "github.com/ibldzn/trs/internal/reporting"
	"github.com/ibldzn/trs/internal/server"
	"github.com/ibldzn/trs/internal/snapshot"
	"github.com/ibldzn/trs/internal/user"
	webfiles "github.com/ibldzn/trs/web"
)

func Run(ctx context.Context) error {
	applicationConfig, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	if err := applicationConfig.ValidateServer(); err != nil {
		return fmt.Errorf("validate server configuration: %w", err)
	}
	location, err := applicationConfig.App.Location()
	if err != nil {
		return fmt.Errorf("load business timezone: %w", err)
	}

	logger := NewLogger(applicationConfig.App.Environment)
	slog.SetDefault(logger)
	logger.Info("application starting",
		"name", applicationConfig.App.Name,
		"environment", applicationConfig.App.Environment,
	)

	databaseContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	databaseConnection, err := database.Open(databaseContext, applicationConfig.Database)
	cancel()
	if err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	defer func() {
		if err := databaseConnection.Close(); err != nil {
			logger.Error("close database", "error", err)
		}
	}()
	logger.Info("database connection initialized",
		"host", applicationConfig.Database.Host,
		"port", applicationConfig.Database.Port,
		"database", applicationConfig.Database.Name,
	)
	externalContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	dwhDatabase, err := database.OpenDSN(externalContext, applicationConfig.DWH.DSN, location)
	cancel()
	if err != nil {
		return fmt.Errorf("initialize DWH: %w", err)
	}
	defer dwhDatabase.Close()
	externalContext, cancel = context.WithTimeout(ctx, 10*time.Second)
	msoDatabase, err := database.OpenDSN(externalContext, applicationConfig.MSO.DSN, location)
	cancel()
	if err != nil {
		return fmt.Errorf("initialize MSO: %w", err)
	}
	defer msoDatabase.Close()
	if applicationConfig.Fincloud.InsecureTLS {
		logger.Warn("Fincloud TLS certificate verification disabled by explicit configuration")
	}
	fincloudClient, err := fincloud.NewClient(fincloud.Config{
		BaseURL: applicationConfig.Fincloud.BaseURL, Username: applicationConfig.Fincloud.Username, Password: applicationConfig.Fincloud.Password,
		LocationID: applicationConfig.Fincloud.LocationID, RoleID: applicationConfig.Fincloud.RoleID, CAFile: applicationConfig.Fincloud.CAFile,
		InsecureTLS: applicationConfig.Fincloud.InsecureTLS, Timeout: applicationConfig.Fincloud.Timeout, MaxReportSize: applicationConfig.Fincloud.MaxReportSize,
	})
	if err != nil {
		return fmt.Errorf("initialize Fincloud: %w", err)
	}
	defer func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := fincloudClient.Close(closeContext); err != nil {
			logger.Warn("close Fincloud session", "error", err)
		}
		closeCancel()
	}()

	bootstrapContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = access.Bootstrap(bootstrapContext, databaseConnection, PermissionDefinitions(), time.Now().UTC())
	cancel()
	if err != nil {
		return fmt.Errorf("initialize access control: %w", err)
	}
	logger.Info("access control initialized")

	userRepository := user.NewRepository(databaseConnection)
	accessRepository := access.NewRepository(databaseConnection)
	sessionRepository := auth.NewSessionRepository(databaseConnection)
	authenticationService, err := browserauth.NewService(
		userRepository,
		accessRepository,
		sessionRepository,
		applicationConfig.Session.Lifetime,
		applicationConfig.Session.RememberLifetime,
		logger,
	)
	if err != nil {
		return fmt.Errorf("initialize browser authentication: %w", err)
	}
	cleanupContext, stopCleanup := context.WithCancel(ctx)
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		auth.RunSessionCleanup(cleanupContext, sessionRepository, time.Hour, logger)
	}()
	defer func() {
		stopCleanup()
		<-cleanupDone
	}()

	var contentFiles fs.FS = webfiles.Files
	reloadTemplates := false
	if applicationConfig.App.IsDevelopment() {
		contentFiles = os.DirFS("web")
		reloadTemplates = true
	}

	renderer, err := render.New(contentFiles, reloadTemplates)
	if err != nil {
		return fmt.Errorf("initialize renderer: %w", err)
	}
	staticFiles, err := fs.Sub(contentFiles, "static")
	if err != nil {
		return fmt.Errorf("initialize static files: %w", err)
	}
	errorResponder := render.NewErrorResponder(renderer, applicationConfig.App.Name, logger)
	appendAudit := func(ctx context.Context, event audit.Event) error {
		return audit.Append(ctx, databaseConnection, event)
	}
	snapshotRepository := snapshot.NewRepository(databaseConnection, location)
	snapshotService := snapshot.NewService(snapshot.NewSource(fincloudClient, location), snapshotRepository, location, appendAudit, logger)
	msoRepository := mso.NewRepository(msoDatabase, location, applicationConfig.MSO.InterestTypeQuery, applicationConfig.MSO.DebtorTypeQuery)
	positionService, err := position.NewService(
		fincloudClient, msoRepository, dwh.NewRepository(dwhDatabase, location),
		snapshotRepository, contractual.Calculator{}, location,
	)
	if err != nil {
		return fmt.Errorf("initialize position service: %w", err)
	}
	reportingManager, err := corereporting.NewManager(ctx, positionService, applicationConfig.Reporting.Concurrency, appendAudit, logger)
	if err != nil {
		return fmt.Errorf("initialize reporting: %w", err)
	}
	defer reportingManager.Close()
	lpsGenerator, err := corelps.NewGenerator(fincloudClient, fincloudClient, msoRepository, corelps.NewHTTPRateProvider(nil), location)
	if err != nil {
		return fmt.Errorf("initialize LPS: %w", err)
	}

	cookieManager := browserauth.NewCookieManager(
		applicationConfig.Session.CookieName,
		applicationConfig.Session.Secure,
		applicationConfig.Session.RememberLifetime,
	)
	authenticationHTTP := browserauth.NewHTTP(
		authenticationService,
		renderer,
		cookieManager,
		applicationConfig.App.Name,
		applicationConfig.App.AllowRegistration,
		logger,
		appendAudit,
		errorResponder,
	)
	navigationRegistry, err := navigation.NewRegistry(navigationGroups(), PermissionDefinitions())
	if err != nil {
		return fmt.Errorf("initialize admin navigation: %w", err)
	}
	adminHTTP := adminshell.New(renderer, navigationRegistry, applicationConfig.App.Name, errorResponder)
	handler := server.NewRouter(server.RouterDependencies{
		StaticFiles:       staticFiles,
		AllowRegistration: applicationConfig.App.AllowRegistration,
		Authentication:    authenticationHTTP,
		RegisterAuthenticated: func(router chi.Router) {
			registerFeatureRoutes(router, featureDependencies{
				database: databaseConnection, users: userRepository, access: accessRepository,
				admin: adminHTTP, cookies: cookieManager,
				positions: positionService, snapshot: snapshotService, reporting: reportingManager, lps: lpsGenerator,
				location: location, maxReportingUpload: applicationConfig.Reporting.MaxUploadBytes,
				lpsDefaultCode: applicationConfig.LPS.DefaultParticipantCode, appendAudit: appendAudit, logger: logger,
			})
		},
		Errors: errorResponder,
	})
	schedulerContext, stopScheduler := context.WithCancel(ctx)
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		snapshot.RunScheduler(schedulerContext, snapshotService, applicationConfig.Snapshot.RefreshInterval, applicationConfig.Snapshot.RefreshOnStart, logger)
	}()
	defer func() { stopScheduler(); <-schedulerDone }()
	httpServer := server.NewHTTPServer(applicationConfig.App.Address(), handler, logger)
	return httpServer.Run(ctx)
}

func NewLogger(environment string) *slog.Logger {
	if environment == "development" {
		return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
