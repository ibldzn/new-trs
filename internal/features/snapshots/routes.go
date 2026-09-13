package snapshots

import "github.com/go-chi/chi/v5"

func (handler *Handler) RegisterRoutes(router chi.Router) {
	router.With(handler.admin.RequirePermission(PermissionView)).Get("/snapshot", handler.Index)
	router.With(handler.admin.RequirePermission(PermissionRefresh)).Post("/snapshot/refresh", handler.Refresh)
}
