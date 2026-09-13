package reporting

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/loan"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	jobqueue "github.com/ibldzn/trs/internal/reporting"
)

type Handler struct {
	admin          *adminshell.Shell
	jobs           *jobqueue.Manager
	location       *time.Location
	maxUploadBytes int64
}

type IndexData struct {
	AsOf  string
	Error string
}

type StatusData struct{ Job jobqueue.Job }

func NewHandler(admin *adminshell.Shell, jobs *jobqueue.Manager, location *time.Location, maxUploadBytes int64) *Handler {
	return &Handler{admin: admin, jobs: jobs, location: location, maxUploadBytes: maxUploadBytes}
}

func (handler *Handler) Index(writer http.ResponseWriter, request *http.Request) {
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/reporting/index", "Bulk Reporting", IndexData{AsOf: loan.NewDate(time.Now(), handler.location).String()})
}

func (handler *Handler) Submit(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, handler.maxUploadBytes)
	if err := request.ParseMultipartForm(handler.maxUploadBytes); err != nil {
		handler.renderInputError(writer, request, http.StatusBadRequest, "Upload a valid CSV within the size limit.")
		return
	}
	defer request.MultipartForm.RemoveAll()
	asOfText := strings.TrimSpace(request.PostFormValue("as_of"))
	asOf, err := loan.ParseDate(asOfText, handler.location)
	if err != nil {
		handler.renderInputError(writer, request, http.StatusUnprocessableEntity, "Enter a valid reporting date.")
		return
	}
	file, _, err := request.FormFile("accounts")
	if err != nil {
		handler.renderInputError(writer, request, http.StatusUnprocessableEntity, "Select an account CSV.")
		return
	}
	defer file.Close()
	accounts, err := readAccounts(file)
	if err != nil || len(accounts) == 0 {
		handler.renderInputError(writer, request, http.StatusUnprocessableEntity, "CSV must contain account numbers in its first column.")
		return
	}
	principal, ok := browserauth.CurrentPrincipal(request.Context())
	if !ok {
		handler.admin.Internal(writer, request, "submit reporting job", errors.New("principal missing"))
		return
	}
	actor := audit.Identity{UserID: principal.Actor.UserID, Username: principal.Actor.Username}
	effective := audit.Identity{UserID: principal.UserID, Username: principal.Username}
	job, err := handler.jobs.Submit(principal.UserID, accounts, asOf, audit.Attribution{Actor: &actor, Effective: &effective})
	if err != nil {
		handler.renderInputError(writer, request, http.StatusUnprocessableEntity, err.Error())
		return
	}
	http.Redirect(writer, request, "/reports/"+job.ID, http.StatusSeeOther)
}

func (handler *Handler) Status(writer http.ResponseWriter, request *http.Request) {
	job, ok := handler.job(writer, request)
	if !ok {
		return
	}
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/reporting/status", "Report Status", StatusData{Job: job})
}

func (handler *Handler) StatusPartial(writer http.ResponseWriter, request *http.Request) {
	job, ok := handler.job(writer, request)
	if !ok {
		return
	}
	page, ok := handler.admin.PageData(request, "Report Status", StatusData{Job: job})
	if !ok {
		handler.admin.Internal(writer, request, "prepare report status", errors.New("principal missing"))
		return
	}
	if err := handler.admin.RenderPartial(writer, http.StatusOK, "features/reporting/status", "report-status", page); err != nil {
		handler.admin.Internal(writer, request, "render report status", err)
	}
}

func (handler *Handler) Download(writer http.ResponseWriter, request *http.Request) {
	principal, _ := browserauth.CurrentPrincipal(request.Context())
	body, job, err := handler.jobs.Download(chi.URLParam(request, "id"), principal.UserID, access.IsAdminRole(principal.RoleSlug))
	if errors.Is(err, jobqueue.ErrJobNotFound) {
		handler.admin.NotFound(writer, request)
		return
	}
	if err != nil {
		http.Error(writer, http.StatusText(http.StatusConflict), http.StatusConflict)
		return
	}
	writer.Header().Set("Content-Type", "text/csv; charset=utf-8")
	writer.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="contractual_report_%s.csv"`, job.AsOf.String()))
	_, _ = writer.Write(body)
}

func (handler *Handler) Cancel(writer http.ResponseWriter, request *http.Request) {
	principal, _ := browserauth.CurrentPrincipal(request.Context())
	if err := handler.jobs.Cancel(chi.URLParam(request, "id"), principal.UserID, access.IsAdminRole(principal.RoleSlug)); errors.Is(err, jobqueue.ErrJobNotFound) {
		handler.admin.NotFound(writer, request)
		return
	}
	http.Redirect(writer, request, "/reports/"+chi.URLParam(request, "id"), http.StatusSeeOther)
}

func (handler *Handler) job(writer http.ResponseWriter, request *http.Request) (jobqueue.Job, bool) {
	principal, ok := browserauth.CurrentPrincipal(request.Context())
	if !ok {
		return jobqueue.Job{}, false
	}
	job, err := handler.jobs.Get(chi.URLParam(request, "id"), principal.UserID, access.IsAdminRole(principal.RoleSlug))
	if errors.Is(err, jobqueue.ErrJobNotFound) {
		handler.admin.NotFound(writer, request)
		return jobqueue.Job{}, false
	}
	if err != nil {
		handler.admin.Internal(writer, request, "read report job", err)
		return jobqueue.Job{}, false
	}
	return job, true
}

func (handler *Handler) renderInputError(writer http.ResponseWriter, request *http.Request, status int, message string) {
	handler.admin.RenderPage(writer, request, status, "features/reporting/index", "Bulk Reporting", IndexData{AsOf: request.PostFormValue("as_of"), Error: message})
}

func readAccounts(reader io.Reader) ([]string, error) {
	csvReader := csv.NewReader(reader)
	csvReader.FieldsPerRecord = -1
	accounts := make([]string, 0)
	for line := 0; ; line++ {
		record, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(record) == 0 {
			return nil, err
		}
		account := strings.TrimSpace(strings.TrimPrefix(record[0], "\uFEFF"))
		if line == 0 && (strings.EqualFold(account, "account_number") || strings.EqualFold(account, "no_rekening")) {
			continue
		}
		if account != "" {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}
