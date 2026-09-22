package browserauth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/render"
)

const maxFormBody = 32 << 10

type authenticationService interface {
	Labels(context.Context) (fincloud.AuthLabels, error)
	Login(context.Context, LoginInput, time.Time) (LoginResult, error)
	ResolveSession(context.Context, [32]byte, time.Time) (Principal, error)
	Logout(context.Context, [32]byte) error
}

type HTTP struct {
	service     authenticationService
	renderer    *render.Renderer
	cookies     CookieManager
	appName     string
	logger      *slog.Logger
	appendAudit func(context.Context, audit.Event) error
	errors      *render.ErrorResponder
}

type LoginForm struct {
	Username         string
	SelectedLocation string
	SelectedRole     string
	Locations        []fincloud.AuthLabel
	Roles            []fincloud.AuthLabel
	RememberMe       bool
	Next             string
	Errors           map[string]string
}

func NewHTTP(service authenticationService, renderer *render.Renderer, cookies CookieManager, appName string, logger *slog.Logger, appendAudit func(context.Context, audit.Event) error, errorResponder *render.ErrorResponder) *HTTP {
	return &HTTP{service: service, renderer: renderer, cookies: cookies, appName: appName, logger: logger, appendAudit: appendAudit, errors: errorResponder}
}

func (handler *HTTP) LoginPage(writer http.ResponseWriter, request *http.Request) {
	form := LoginForm{Next: SafeRedirect(request.URL.Query().Get("next")), Errors: map[string]string{}}
	if !handler.loadLabels(writer, request, &form) {
		return
	}
	handler.renderLogin(writer, request, http.StatusOK, form)
}

func (handler *HTTP) Login(writer http.ResponseWriter, request *http.Request) {
	if !parseForm(writer, request) {
		return
	}
	form := LoginForm{
		Username: strings.TrimSpace(request.PostFormValue("username")), SelectedLocation: strings.TrimSpace(request.PostFormValue("locationid")),
		SelectedRole: strings.TrimSpace(request.PostFormValue("roleid")), RememberMe: request.PostFormValue("remember_me") != "",
		Next: SafeRedirect(request.PostFormValue("next")), Errors: map[string]string{},
	}
	password := request.PostFormValue("password")
	if form.Username == "" {
		form.Errors["username"] = "username must not be empty"
	}
	if password == "" {
		form.Errors["password"] = "password must not be empty"
	} else if len(password) > maxPasswordBytes {
		form.Errors["password"] = "password is too long"
	}
	if form.SelectedLocation == "" {
		form.Errors["locationid"] = "select a Fincloud location"
	}
	if form.SelectedRole == "" {
		form.Errors["roleid"] = "select a Fincloud role"
	}
	if len(form.Errors) != 0 {
		if handler.loadLabels(writer, request, &form) {
			handler.renderLogin(writer, request, http.StatusUnprocessableEntity, form)
		}
		return
	}

	result, err := handler.service.Login(request.Context(), LoginInput{Username: form.Username, Password: password, LocationID: form.SelectedLocation, RoleID: form.SelectedRole, RememberMe: form.RememberMe}, time.Now().UTC())
	if errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrInactiveUser) {
		handler.appendBestEffortAudit(request, audit.Event{Action: audit.ActionAuthLoginFailed, Metadata: audit.LoginFailedMetadata{Username: form.Username}, CreatedAt: time.Now().UTC()})
		form.Errors["credentials"] = "Authentication failed. Check your Fincloud credentials, location, role, and THOR access."
		if handler.loadLabels(writer, request, &form) {
			handler.renderLogin(writer, request, http.StatusUnprocessableEntity, form)
		}
		return
	}
	if err != nil {
		handler.internalError(writer, request, "Fincloud login", err)
		return
	}
	identity := audit.Identity{UserID: result.User.ID, Username: result.User.Username}
	if result.Provisioned {
		handler.appendBestEffortAudit(request, audit.Event{Attribution: audit.Attribution{SystemActor: "system:fincloud-auth"}, Action: audit.ActionUserAutoProvisioned, Resource: audit.ResourceUser, ResourceID: result.User.ID, CreatedAt: time.Now().UTC()})
	}
	handler.appendBestEffortAudit(request, audit.Event{Attribution: audit.Attribution{Actor: &identity, Effective: &identity}, Action: audit.ActionAuthLogin, Resource: audit.ResourceUser, ResourceID: identity.UserID, CreatedAt: time.Now().UTC()})
	handler.cookies.Set(writer, result.RawToken, result.Session.RememberMe, result.Session.CreatedAt)
	http.Redirect(writer, request, form.Next, http.StatusSeeOther)
}

func (handler *HTTP) Logout(writer http.ResponseWriter, request *http.Request) {
	principal, hasPrincipal := CurrentPrincipal(request.Context())
	rawToken, err := handler.cookies.Read(request)
	if errors.Is(err, http.ErrNoCookie) || err == nil && !validToken(rawToken) {
		handler.cookies.Clear(writer)
		http.Redirect(writer, request, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		handler.internalError(writer, request, "read logout cookie", err)
		return
	}
	if err := handler.service.Logout(request.Context(), auth.HashToken(rawToken)); err != nil {
		handler.internalError(writer, request, "logout", err)
		return
	}
	if hasPrincipal {
		handler.appendBestEffortAudit(request, audit.Event{Attribution: auditAttributionFromPrincipal(principal), Action: audit.ActionAuthLogout, Resource: audit.ResourceUser, ResourceID: principal.UserID, CreatedAt: time.Now().UTC()})
	}
	handler.cookies.Clear(writer)
	http.Redirect(writer, request, "/login", http.StatusSeeOther)
}

func (handler *HTTP) loadLabels(writer http.ResponseWriter, request *http.Request, form *LoginForm) bool {
	labels, err := handler.service.Labels(request.Context())
	if err != nil {
		handler.internalError(writer, request, "load Fincloud login options", err)
		return false
	}
	form.Locations, form.Roles = labels.Locations, labels.Roles
	return true
}

func (handler *HTTP) appendBestEffortAudit(request *http.Request, event audit.Event) {
	if handler.appendAudit == nil {
		return
	}
	if err := handler.appendAudit(request.Context(), event); err != nil {
		handler.logger.WarnContext(request.Context(), "append authentication audit", "request_id", middleware.GetReqID(request.Context()), "method", request.Method, "path", request.URL.Path, "action", event.Action, "error", err)
	}
}

func (handler *HTTP) renderLogin(writer http.ResponseWriter, request *http.Request, status int, form LoginForm) {
	data := render.PageData{Title: "Login", AppName: handler.appName, Notice: render.NoticeFromID(request.URL.Query().Get("notice")), Data: form}
	if err := handler.renderer.RenderPageWithLayout(writer, status, "login", "auth", data); err != nil {
		handler.internalError(writer, request, "render login page", err)
	}
}

func (handler *HTTP) internalError(writer http.ResponseWriter, request *http.Request, operation string, err error) {
	handler.errors.Internal(writer, request, operation, err)
}

func parseForm(writer http.ResponseWriter, request *http.Request) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxFormBody)
	if err := request.ParseForm(); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(writer, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(writer, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		}
		return false
	}
	return true
}
