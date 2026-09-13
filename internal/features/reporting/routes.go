package reporting

import "github.com/go-chi/chi/v5"

func (handler *Handler) RegisterRoutes(router chi.Router) {
	permission := handler.admin.RequirePermission(PermissionGenerate)
	router.With(permission).Get("/reports", handler.Index)
	router.With(permission).Post("/reports", handler.Submit)
	router.With(permission).Get("/reports/{id}", handler.Status)
	router.With(permission).Get("/reports/{id}/status", handler.StatusPartial)
	router.With(permission).Get("/reports/{id}/download", handler.Download)
	router.With(permission).Post("/reports/{id}/cancel", handler.Cancel)
}
