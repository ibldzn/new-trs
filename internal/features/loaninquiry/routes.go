package loaninquiry

import "github.com/go-chi/chi/v5"

func (handler *Handler) RegisterRoutes(router chi.Router) {
	router.With(handler.admin.RequirePermission(PermissionInquiry)).Get("/loans", handler.Index)
	router.With(handler.admin.RequirePermission(PermissionInquiry)).Post("/loans/inquiry", handler.Inquiry)
}
