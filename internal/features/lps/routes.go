package lps

import "github.com/go-chi/chi/v5"

func (handler *Handler) RegisterRoutes(router chi.Router) {
	permission := handler.admin.RequirePermission(PermissionGenerate)
	router.With(permission).Get("/lps", handler.Index)
	router.With(permission).Post("/lps", handler.Generate)
}
