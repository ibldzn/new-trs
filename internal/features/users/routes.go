package users

import "github.com/go-chi/chi/v5"

func (handler *Handler) RegisterRoutes(router chi.Router) {
	permission := handler.admin.RequirePermission(PermissionManage)
	router.With(permission).Get("/access", handler.Index)
	router.With(permission).Get("/access/{id}", handler.Show)
	router.With(permission).Post("/access/{id}/permissions", handler.ReplacePermissions)
	router.With(permission).Post("/access/{id}/activate", handler.Activate)
	router.With(permission).Post("/access/{id}/deactivate", handler.Deactivate)
}
