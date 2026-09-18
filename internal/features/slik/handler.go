package slik

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/loan"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	core "github.com/ibldzn/trs/internal/slik"
)

type Handler struct {
	admin          *adminshell.Shell
	jobs           *core.Manager
	location       *time.Location
	maxUploadBytes int64
}
type IndexData struct {
	AsOf, Error string
	Jobs        []core.Job
}
type StatusData struct{ Job core.Job }

func NewHandler(admin *adminshell.Shell, jobs *core.Manager, location *time.Location, maxUploadBytes int64) *Handler {
	return &Handler{admin: admin, jobs: jobs, location: location, maxUploadBytes: maxUploadBytes}
}

func (handler *Handler) Index(writer http.ResponseWriter, request *http.Request) {
	principal, _ := browserauth.CurrentPrincipal(request.Context())
	jobs, err := handler.jobs.History(request.Context(), principal.UserID, access.IsAdminRole(principal.RoleSlug))
	if err != nil {
		handler.admin.Internal(writer, request, "list SLIK jobs", err)
		return
	}
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/slik/index", "SLIK Generator", IndexData{AsOf: loan.NewDate(time.Now(), handler.location).String(), Jobs: jobs})
}

func (handler *Handler) Submit(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, handler.maxUploadBytes+(1<<20))
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		handler.inputError(writer, request, http.StatusBadRequest, "Upload a valid XLSX within the size limit.")
		return
	}
	defer request.MultipartForm.RemoveAll()
	asOf, err := loan.ParseDate(strings.TrimSpace(request.PostFormValue("as_of")), handler.location)
	if err != nil || asOf.After(loan.NewDate(time.Now(), handler.location)) {
		handler.inputError(writer, request, http.StatusUnprocessableEntity, "Enter a valid reporting date up to today.")
		return
	}
	file, header, err := request.FormFile("workbook")
	if err != nil {
		handler.inputError(writer, request, http.StatusUnprocessableEntity, "Select a SLIK .xlsx workbook.")
		return
	}
	defer file.Close()
	principal, ok := browserauth.CurrentPrincipal(request.Context())
	if !ok {
		handler.admin.Internal(writer, request, "submit SLIK job", errors.New("principal missing"))
		return
	}
	actor := audit.Identity{UserID: principal.Actor.UserID, Username: principal.Actor.Username}
	effective := audit.Identity{UserID: principal.UserID, Username: principal.Username}
	job, err := handler.jobs.Submit(request.Context(), audit.Attribution{Actor: &actor, Effective: &effective}, header.Filename, asOf, file)
	if err != nil {
		var validation core.ValidationError
		if errors.As(err, &validation) {
			handler.inputError(writer, request, http.StatusUnprocessableEntity, validation.Error())
			return
		}
		handler.admin.Internal(writer, request, "submit SLIK job", err)
		return
	}
	http.Redirect(writer, request, "/slik/"+job.ID, http.StatusSeeOther)
}

func (handler *Handler) Status(writer http.ResponseWriter, request *http.Request) {
	job, ok := handler.job(writer, request)
	if !ok {
		return
	}
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/slik/status", "SLIK Status", StatusData{Job: job})
}

func (handler *Handler) StatusPartial(writer http.ResponseWriter, request *http.Request) {
	job, ok := handler.job(writer, request)
	if !ok {
		return
	}
	page, ok := handler.admin.PageData(request, "SLIK Status", StatusData{Job: job})
	if !ok {
		handler.admin.Internal(writer, request, "prepare SLIK status", errors.New("principal missing"))
		return
	}
	if err := handler.admin.RenderPartial(writer, http.StatusOK, "features/slik/status", "slik-status", page); err != nil {
		handler.admin.Internal(writer, request, "render SLIK status", err)
	}
}

func (handler *Handler) Download(writer http.ResponseWriter, request *http.Request) {
	principal, _ := browserauth.CurrentPrincipal(request.Context())
	file, job, err := handler.jobs.OpenOutput(request.Context(), chi.URLParam(request, "id"), principal.UserID, access.IsAdminRole(principal.RoleSlug))
	if errors.Is(err, core.ErrNotFound) {
		handler.admin.NotFound(writer, request)
		return
	}
	if errors.Is(err, core.ErrExpired) {
		http.Error(writer, "SLIK output expired", http.StatusGone)
		return
	}
	if errors.Is(err, core.ErrNotReady) {
		http.Error(writer, "SLIK output unavailable", http.StatusConflict)
		return
	}
	if err != nil {
		handler.admin.Internal(writer, request, "open SLIK output", err)
		return
	}
	defer file.Close()
	writer.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	writer.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="slik_%s.xlsx"`, job.AsOf))
	http.ServeContent(writer, request, "slik.xlsx", job.CreatedAt, file)
}

func (handler *Handler) Cancel(writer http.ResponseWriter, request *http.Request) {
	principal, _ := browserauth.CurrentPrincipal(request.Context())
	err := handler.jobs.Cancel(request.Context(), chi.URLParam(request, "id"), principal.UserID, access.IsAdminRole(principal.RoleSlug))
	if errors.Is(err, core.ErrNotFound) {
		handler.admin.NotFound(writer, request)
		return
	}
	if err != nil {
		handler.admin.Internal(writer, request, "cancel SLIK job", err)
		return
	}
	http.Redirect(writer, request, "/slik/"+chi.URLParam(request, "id"), http.StatusSeeOther)
}

func (handler *Handler) job(writer http.ResponseWriter, request *http.Request) (core.Job, bool) {
	principal, _ := browserauth.CurrentPrincipal(request.Context())
	job, err := handler.jobs.Get(request.Context(), chi.URLParam(request, "id"), principal.UserID, access.IsAdminRole(principal.RoleSlug))
	if errors.Is(err, core.ErrNotFound) {
		handler.admin.NotFound(writer, request)
		return core.Job{}, false
	}
	if err != nil {
		handler.admin.Internal(writer, request, "read SLIK job", err)
		return core.Job{}, false
	}
	return job, true
}

func (handler *Handler) inputError(writer http.ResponseWriter, request *http.Request, status int, message string) {
	principal, _ := browserauth.CurrentPrincipal(request.Context())
	jobs, _ := handler.jobs.History(request.Context(), principal.UserID, access.IsAdminRole(principal.RoleSlug))
	handler.admin.RenderPage(writer, request, status, "features/slik/index", "SLIK Generator", IndexData{AsOf: request.PostFormValue("as_of"), Error: message, Jobs: jobs})
}
