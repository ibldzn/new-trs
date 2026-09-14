package loaninquiry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/loan"
	"github.com/ibldzn/trs/internal/platform/adminshell"
)

const maxFormBody = 32 << 10

type positionService interface {
	GetLoanPosition(context.Context, string, loan.Date) (loan.ResolvedPosition, error)
}

type Handler struct {
	admin       *adminshell.Shell
	positions   positionService
	location    *time.Location
	appendAudit func(context.Context, audit.Event) error
	logger      *slog.Logger
}

type PageData struct {
	Account string
	AsOf    string
	Error   string
	Result  *ResultView
}

type ResultView struct {
	Loan                     loan.ContractData
	Position                 loan.LoanPosition
	PrincipalOutstanding     string
	PrincipalDue             string
	InterestDue              string
	PenaltyDue               string
	ContractPrincipal        string
	ReferenceRate            string
	FlatRate                 string
	PeriodicPrincipal        string
	PeriodicInterest         string
	EarlyTerminationEstimate string
	DueDateCount             int
	ContractualSchedule      []ScheduleRowView
}

type ScheduleRowView struct {
	Number           int
	DueDate          string
	Principal        string
	Interest         string
	Installment      string
	ScheduledBalance string
}

func NewHandler(admin *adminshell.Shell, positions positionService, location *time.Location, appendAudit func(context.Context, audit.Event) error, logger *slog.Logger) *Handler {
	return &Handler{admin: admin, positions: positions, location: location, appendAudit: appendAudit, logger: logger}
}

func (handler *Handler) Index(writer http.ResponseWriter, request *http.Request) {
	today := loan.NewDate(time.Now(), handler.location).String()
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/loaninquiry/index", "Loan Inquiry", PageData{AsOf: today})
}

func (handler *Handler) Inquiry(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxFormBody)
	if err := request.ParseForm(); err != nil {
		http.Error(writer, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	data := PageData{Account: strings.TrimSpace(request.PostFormValue("account_number")), AsOf: strings.TrimSpace(request.PostFormValue("as_of"))}
	asOf, err := loan.ParseDate(data.AsOf, handler.location)
	if data.Account == "" || err != nil {
		data.Error = "Enter an account number and valid reporting date."
		handler.admin.RenderPage(writer, request, http.StatusUnprocessableEntity, "features/loaninquiry/index", "Loan Inquiry", data)
		return
	}
	resolved, err := handler.positions.GetLoanPosition(request.Context(), data.Account, asOf)
	if err != nil {
		status, message := inquiryError(err)
		data.Error = message
		handler.admin.RenderPage(writer, request, status, "features/loaninquiry/index", "Loan Inquiry", data)
		return
	}
	view, err := newResultView(resolved)
	if err != nil {
		handler.admin.Internal(writer, request, "prepare loan inquiry", err)
		return
	}
	data.Result = &view
	principal, ok := browserauth.CurrentPrincipal(request.Context())
	if ok && handler.appendAudit != nil {
		actor := audit.Identity{UserID: principal.Actor.UserID, Username: principal.Actor.Username}
		effective := audit.Identity{UserID: principal.UserID, Username: principal.Username}
		event := audit.Event{
			Attribution: audit.Attribution{Actor: &actor, Effective: &effective}, Action: audit.ActionLoanInquiry,
			Metadata: audit.LoanInquiryMetadata{AccountNumber: resolved.Position.AccountNumber, AsOf: asOf.String(), Source: string(resolved.Position.Source)}, CreatedAt: time.Now().UTC(),
		}
		if err := handler.appendAudit(request.Context(), event); err != nil && handler.logger != nil {
			handler.logger.WarnContext(request.Context(), "append loan inquiry audit", "error", err)
		}
	}
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/loaninquiry/index", "Loan Inquiry", data)
}

func newResultView(resolved loan.ResolvedPosition) (ResultView, error) {
	var principalPerPeriod, interestPerPeriod loan.Money
	if resolved.Position.Source == loan.SourceReconstructed {
		if len(resolved.ContractualSchedule) == 0 {
			return ResultView{}, fmt.Errorf("%w: reconstructed contractual schedule is empty", loan.ErrInvariant)
		}
		principalPerPeriod = resolved.ContractualSchedule[0].Principal
		interestPerPeriod = resolved.ContractualSchedule[0].Interest
	} else {
		var err error
		principalPerPeriod, err = resolved.Loan.PlafondLimit.DivInt(int64(resolved.Loan.TenorMonths))
		if err != nil {
			return ResultView{}, err
		}
		interestPerPeriod = resolved.Loan.PlafondLimit.Mul(resolved.Loan.FlatRatePercent)
		interestPerPeriod, err = interestPerPeriod.DivInt(1200)
		if err != nil {
			return ResultView{}, err
		}
	}
	early := principalPerPeriod.Add(interestPerPeriod).Mul(loan.MoneyFromInt(6))
	schedule := make([]ScheduleRowView, 0, len(resolved.ContractualSchedule))
	if resolved.Position.Source == loan.SourceReconstructed {
		for _, row := range resolved.ContractualSchedule {
			schedule = append(schedule, ScheduleRowView{
				Number: row.Number, DueDate: row.DueDate.Time(time.UTC).Format("02/01/2006"),
				Principal: row.Principal.Format(2), Interest: row.Interest.Format(2), Installment: row.Installment.Format(2), ScheduledBalance: row.ScheduledBalance.Format(2),
			})
		}
	}
	return ResultView{
		Loan: resolved.Loan, Position: resolved.Position, PrincipalOutstanding: resolved.Position.PrincipalOutstanding.Format(2),
		PrincipalDue: resolved.Position.PrincipalDue.Format(2), InterestDue: resolved.Position.InterestDue.Format(2),
		PenaltyDue:        resolved.Loan.PenaltyDue.Format(2),
		ContractPrincipal: resolved.Loan.PlafondLimit.Format(2), ReferenceRate: resolved.Loan.ReferenceRatePercent.Format(2),
		FlatRate: resolved.Loan.FlatRatePercent.Format(2), PeriodicPrincipal: principalPerPeriod.Format(2),
		PeriodicInterest: interestPerPeriod.Format(2), EarlyTerminationEstimate: early.Format(2),
		DueDateCount: len(resolved.Loan.ContractSchedule), ContractualSchedule: schedule,
	}, nil
}

func inquiryError(err error) (int, string) {
	switch {
	case errors.Is(err, loan.ErrInvalidInput):
		return http.StatusUnprocessableEntity, err.Error()
	case errors.Is(err, loan.ErrUnsupportedCalculation):
		return http.StatusUnprocessableEntity, "Contractual reconstruction is not supported for this account."
	case errors.Is(err, loan.ErrAmbiguousAccountResolution):
		return http.StatusConflict, "Alternate account resolves to multiple primary accounts."
	case errors.Is(err, loan.ErrCurrentSnapshot):
		return http.StatusServiceUnavailable, "Current-day snapshot is unavailable."
	case errors.Is(err, loan.ErrHistoricalEvidence):
		return http.StatusServiceUnavailable, "Required historical evidence is unavailable."
	case errors.Is(err, loan.ErrFincloudCredentials), errors.Is(err, loan.ErrFincloudSession), errors.Is(err, loan.ErrFincloudUnavailable), errors.Is(err, loan.ErrDWHUnavailable), errors.Is(err, loan.ErrMSOUnavailable):
		return http.StatusServiceUnavailable, "Required banking service is unavailable."
	case errors.Is(err, loan.ErrNotFound):
		return http.StatusNotFound, "Loan account was not found."
	default:
		return http.StatusInternalServerError, "Loan inquiry failed."
	}
}
