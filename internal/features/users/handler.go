package users

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	"github.com/ibldzn/trs/internal/platform/pagination"
	"github.com/ibldzn/trs/internal/platform/webutil"
	"github.com/ibldzn/trs/internal/securityctx"
)

const maxManagementFormBody = 32 << 10

type service interface {
	List(context.Context, string, int) (UserPage, error)
	Find(context.Context, uint64) (Detail, error)
	ReplacePermissions(context.Context, securityctx.Requester, uint64, []string, time.Time) error
	SetActive(context.Context, securityctx.Requester, uint64, bool, time.Time) error
}

type Handler struct {
	admin   *adminshell.Shell
	service service
}

type ListData struct {
	Rows        []UserRecord
	Query       string
	Pagination  pagination.Page
	PreviousURL string
	NextURL     string
}

type ConflictData struct{ Message, BackURL string }

func NewHandler(admin *adminshell.Shell, service service) *Handler {
	return &Handler{admin: admin, service: service}
}

func (handler *Handler) Index(writer http.ResponseWriter, request *http.Request) {
	page, err := handler.service.List(request.Context(), request.URL.Query().Get("q"), queryPage(request))
	if err != nil {
		handler.admin.Internal(writer, request, "list access users", err)
		return
	}
	data := ListData{Rows: page.Users, Query: page.Query, Pagination: page.Pagination, PreviousURL: pageURL(page.Query, page.Pagination.Previous), NextURL: pageURL(page.Query, page.Pagination.Next)}
	pageData, ok := handler.admin.PageData(request, "Access Management", data)
	if !ok {
		handler.admin.Internal(writer, request, "prepare access management", errors.New("principal missing"))
		return
	}
	if request.Header.Get("HX-Request") == "true" {
		if err := handler.admin.RenderPartial(writer, http.StatusOK, "features/users/index", "users-table", pageData); err != nil {
			handler.admin.Internal(writer, request, "render access users", err)
		}
		return
	}
	if err := handler.admin.RenderPartial(writer, http.StatusOK, "features/users/index", "admin", pageData); err != nil {
		handler.admin.Internal(writer, request, "render access management", err)
	}
}

func (handler *Handler) Show(writer http.ResponseWriter, request *http.Request) {
	id, ok := handler.routeID(writer, request)
	if !ok {
		return
	}
	detail, err := handler.service.Find(request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		handler.admin.NotFound(writer, request)
		return
	}
	if err != nil {
		handler.admin.Internal(writer, request, "show access user", err)
		return
	}
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/users/show", "Access Management", detail)
}

func (handler *Handler) ReplacePermissions(writer http.ResponseWriter, request *http.Request) {
	id, ok := handler.routeID(writer, request)
	if !ok || !parseManagementForm(writer, request) {
		return
	}
	principal, ok := browserauth.CurrentPrincipal(request.Context())
	if !ok {
		handler.admin.Internal(writer, request, "replace access permissions", errors.New("principal missing"))
		return
	}
	err := handler.service.ReplacePermissions(request.Context(), principal.SecurityContext(), id, request.PostForm["permission"], time.Now().UTC())
	if handler.handleMutationError(writer, request, err, fmt.Sprintf("/access/%d", id)) {
		return
	}
	http.Redirect(writer, request, fmt.Sprintf("/access/%d?notice=permissions-updated", id), http.StatusSeeOther)
}

func (handler *Handler) Activate(writer http.ResponseWriter, request *http.Request) {
	handler.setActive(writer, request, true)
}
func (handler *Handler) Deactivate(writer http.ResponseWriter, request *http.Request) {
	handler.setActive(writer, request, false)
}

func (handler *Handler) setActive(writer http.ResponseWriter, request *http.Request, active bool) {
	id, ok := handler.routeID(writer, request)
	if !ok {
		return
	}
	principal, ok := browserauth.CurrentPrincipal(request.Context())
	if !ok {
		handler.admin.Internal(writer, request, "change access user status", errors.New("principal missing"))
		return
	}
	err := handler.service.SetActive(request.Context(), principal.SecurityContext(), id, active, time.Now().UTC())
	if handler.handleMutationError(writer, request, err, fmt.Sprintf("/access/%d", id)) {
		return
	}
	http.Redirect(writer, request, fmt.Sprintf("/access/%d?notice=user-status-updated", id), http.StatusSeeOther)
}

func (handler *Handler) handleMutationError(writer http.ResponseWriter, request *http.Request, err error, backURL string) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound):
		handler.admin.NotFound(writer, request)
	case errors.Is(err, ErrLastAccessManager):
		handler.admin.RenderPage(writer, request, http.StatusConflict, "conflict", "Conflict", ConflictData{Message: "At least one active access manager must remain.", BackURL: backURL})
	case errors.Is(err, ErrUnknownPermission), errors.Is(err, ErrBaselinePermission):
		http.Error(writer, http.StatusText(http.StatusUnprocessableEntity), http.StatusUnprocessableEntity)
	default:
		handler.admin.Internal(writer, request, "update access user", err)
	}
	return true
}

func (handler *Handler) routeID(writer http.ResponseWriter, request *http.Request) (uint64, bool) {
	id, ok := webutil.RouteID(request)
	if !ok {
		handler.admin.NotFound(writer, request)
	}
	return id, ok
}

func queryPage(request *http.Request) int {
	page, err := strconv.Atoi(request.URL.Query().Get("page"))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

func pageURL(query string, page int) string {
	if page == 0 {
		return ""
	}
	values := url.Values{"page": {strconv.Itoa(page)}}
	if query != "" {
		values.Set("q", query)
	}
	return "/access?" + values.Encode()
}

func parseManagementForm(writer http.ResponseWriter, request *http.Request) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, maxManagementFormBody)
	if err := request.ParseForm(); err != nil {
		http.Error(writer, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return false
	}
	return true
}
