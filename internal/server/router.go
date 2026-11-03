package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ibldzn/trs/internal/branding"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/render"
)

type RouterDependencies struct {
	StaticFiles           fs.FS
	Branding              *branding.Handler
	Authentication        *browserauth.HTTP
	RegisterAPI           func(chi.Router)
	RegisterAuthenticated func(chi.Router)
	Ready                 func(context.Context) error
	Errors                *render.ErrorResponder
	Logger                *slog.Logger
}

func NewRouter(dependencies RouterDependencies) http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(accessLog(dependencies.Logger))
	router.Use(dependencies.Errors.Recoverer)
	router.Use(securityHeaders)
	router.NotFound(dependencies.Errors.NotFound)

	router.Get("/health", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	router.Get("/ready", func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
		defer cancel()
		if dependencies.Ready == nil || dependencies.Ready(ctx) != nil {
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte("{\"status\":\"not ready\"}\n"))
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("{\"status\":\"ready\"}\n"))
	})
	if dependencies.RegisterAPI != nil {
		router.Route("/api/v1", func(api chi.Router) {
			api.Use(noStore)
			dependencies.RegisterAPI(api)
		})
	}

	router.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(dependencies.StaticFiles))))
	if dependencies.Branding != nil {
		router.Get("/branding/logo", dependencies.Branding.Logo)
		router.Get("/branding/logo-wide", dependencies.Branding.WideLogo)
		router.Get("/branding/favicon", dependencies.Branding.Favicon)
	}

	router.Group(func(web chi.Router) {
		web.Use(noStore)
		web.Use(http.NewCrossOriginProtection().Handler)
		web.Use(dependencies.Authentication.LoadPrincipal)

		web.Group(func(guest chi.Router) {
			guest.Use(dependencies.Authentication.RequireGuest)
			guest.Get("/login", dependencies.Authentication.LoginPage)
			guest.Post("/login", dependencies.Authentication.Login)
		})
		web.Post("/logout", dependencies.Authentication.Logout)

		web.Group(func(authenticated chi.Router) {
			authenticated.Use(dependencies.Authentication.RequireAuth)
			if dependencies.RegisterAuthenticated != nil {
				dependencies.RegisterAuthenticated(authenticated)
			}
		})
	})

	return router
}
