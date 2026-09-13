package lps

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/loan"
	corelps "github.com/ibldzn/trs/internal/lps"
	"github.com/ibldzn/trs/internal/platform/adminshell"
)

const maxFormBody = 32 << 10

type generator interface {
	Generate(context.Context, corelps.Input, io.Writer) (corelps.Result, error)
}

type Handler struct {
	admin       *adminshell.Shell
	generator   generator
	defaultCode string
	appendAudit func(context.Context, audit.Event) error
	logger      *slog.Logger
	location    *time.Location
}

type PageData struct {
	Input corelps.Input
	Error string
}

func NewHandler(admin *adminshell.Shell, generator generator, defaultCode string, location *time.Location, appendAudit func(context.Context, audit.Event) error, logger *slog.Logger) *Handler {
	return &Handler{admin: admin, generator: generator, defaultCode: defaultCode, location: location, appendAudit: appendAudit, logger: logger}
}

func (handler *Handler) Index(writer http.ResponseWriter, request *http.Request) {
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/lps/index", "LPS Generation", PageData{Input: corelps.Input{ParticipantCode: handler.defaultCode, ReportingDate: time.Now().In(handler.location).Format("20060102")}})
}

func (handler *Handler) Generate(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxFormBody)
	if err := request.ParseForm(); err != nil {
		http.Error(writer, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	input := corelps.Input{
		ParticipantCode: strings.TrimSpace(request.PostFormValue("participant_code")), ReportingDate: strings.TrimSpace(request.PostFormValue("reporting_date")),
		Period: strings.TrimSpace(request.PostFormValue("period")), Version: strings.TrimSpace(request.PostFormValue("version")),
	}
	file, err := os.CreateTemp("", "trs-lps-*.zip")
	if err != nil {
		handler.admin.Internal(writer, request, "create LPS artifact", err)
		return
	}
	defer os.Remove(file.Name())
	defer file.Close()
	result, err := handler.generator.Generate(request.Context(), input, file)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, loan.ErrInvalidInput) {
			status = http.StatusUnprocessableEntity
		}
		handler.admin.RenderPage(writer, request, status, "features/lps/index", "LPS Generation", PageData{Input: input, Error: safeGenerationError(err)})
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		handler.admin.Internal(writer, request, "seek LPS artifact", err)
		return
	}
	principal, ok := browserauth.CurrentPrincipal(request.Context())
	if !ok {
		handler.admin.Internal(writer, request, "audit LPS artifact", errors.New("principal missing"))
		return
	}
	if handler.appendAudit != nil {
		actor := audit.Identity{UserID: principal.Actor.UserID, Username: principal.Actor.Username}
		effective := audit.Identity{UserID: principal.UserID, Username: principal.Username}
		err := handler.appendAudit(request.Context(), audit.Event{
			Attribution: audit.Attribution{Actor: &actor, Effective: &effective}, Action: audit.ActionLPSGenerate,
			Metadata: audit.LPSMetadata{ParticipantCode: input.ParticipantCode, ReportingDate: input.ReportingDate, RowCount: result.DNRows + result.DSNRows + result.DKRows}, CreatedAt: time.Now().UTC(),
		})
		if err != nil && handler.logger != nil {
			handler.logger.WarnContext(request.Context(), "append LPS audit", "error", err)
		}
	}
	writer.Header().Set("Content-Type", "application/zip")
	writer.Header().Set("Content-Disposition", `attachment; filename="`+result.Filename+`"`)
	_, _ = io.Copy(writer, file)
}

func safeGenerationError(err error) string {
	if errors.Is(err, context.Canceled) {
		return "LPS generation was canceled."
	}
	return "LPS generation failed because mandatory source data is unavailable or malformed."
}
