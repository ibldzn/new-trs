package slik

import "github.com/go-chi/chi/v5"

func (handler *Handler) RegisterRoutes(router chi.Router) {
	permission := handler.admin.RequirePermission(PermissionGenerate)
	router.With(permission).Get("/slik", handler.Index)
	router.With(permission).Post("/slik", handler.Submit)
	router.With(permission).Get("/slik/{id}", handler.Status)
	router.With(permission).Get("/slik/{id}/status", handler.StatusPartial)
	router.With(permission).Get("/slik/{id}/download", handler.Download)
	router.With(permission).Post("/slik/{id}/cancel", handler.Cancel)
}
