package loaninquiry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
	ReportingDate            string
	PeriodLabel              string
	CustomerName             string
	PrimaryAccount           string
	AlternateAccount         string
	Branch                   string
	Product                  string
	LoanPeriod               string
	InstallmentSummary       string
	PrincipalOutstanding     string
	PrincipalDue             string
	InterestDue              string
	PenaltyDue               string
	ContractPrincipal        string
	ReferenceRate            string
	FlatRate                 string
	Collectability           string
	EarlyTerminationEstimate string
	IsReconstructed          bool
	PrintURL                 string
	ScheduleRows             []ScheduleRowView
}

type ScheduleRowView struct {
	Number      int
	Date        string
	Installment string
	Principal   string
	Interest    string
	Outstanding string
	Status      string
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
	resolved, view, err := handler.resolveResult(request.Context(), data.Account, asOf)
	if err != nil {
		status, message := inquiryError(err)
		data.Error = message + " " + err.Error()
		handler.admin.RenderPage(writer, request, status, "features/loaninquiry/index", "Loan Inquiry", data)
		return
	}
	data.Result = &view
	handler.auditInquiry(request.Context(), resolved, asOf)
	handler.admin.RenderPage(writer, request, http.StatusOK, "features/loaninquiry/index", "Loan Inquiry", data)
}

func (handler *Handler) resolveResult(ctx context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, ResultView, error) {
	resolved, err := handler.positions.GetLoanPosition(ctx, account, asOf)
	if err != nil {
		return loan.ResolvedPosition{}, ResultView{}, err
	}
	view, err := newResultView(resolved, account, asOf)
	return resolved, view, err
}

func (handler *Handler) auditInquiry(ctx context.Context, resolved loan.ResolvedPosition, asOf loan.Date) {
	principal, ok := browserauth.CurrentPrincipal(ctx)
	if ok && handler.appendAudit != nil {
		identity := audit.Identity{UserID: principal.UserID, Username: principal.Username}
		event := audit.Event{
			Attribution: audit.Attribution{Actor: &identity, Effective: &identity}, Action: audit.ActionLoanInquiry,
			Metadata: audit.LoanInquiryMetadata{AccountNumber: resolved.Position.AccountNumber, AsOf: asOf.String(), Source: string(resolved.Position.Source)}, CreatedAt: time.Now().UTC(),
		}
		if err := handler.appendAudit(ctx, event); err != nil && handler.logger != nil {
			handler.logger.WarnContext(ctx, "append loan inquiry audit", "error", err)
		}
	}
}

func newResultView(resolved loan.ResolvedPosition, requestedAccount string, asOf loan.Date) (ResultView, error) {
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
	schedule := make([]ScheduleRowView, 0, len(resolved.ContractualSchedule)+1)
	if !resolved.Position.LoanStartDate.IsZero() {
		schedule = append(schedule, ScheduleRowView{
			Number: 0, Date: formatDate(resolved.Position.LoanStartDate), Installment: formatCurrency(loan.Money{}),
			Principal: formatCurrency(loan.Money{}), Interest: formatCurrency(loan.Money{}),
			Outstanding: formatCurrency(resolved.Loan.PlafondLimit), Status: "DISBURSED",
		})
	}
	if resolved.Position.Source == loan.SourceReconstructed {
		paymentStatusByInstallment := make(map[int64]string, len(resolved.Loan.ContractScheduleEvidence))
		for _, row := range resolved.Loan.ContractScheduleEvidence {
			paymentStatusByInstallment[row.Number] = row.RawPaymentStatus
		}
		for _, row := range resolved.ContractualSchedule {
			schedule = append(schedule, ScheduleRowView{
				Number: row.Number, Date: formatDate(row.DueDate), Principal: formatCurrency(row.Principal),
				Interest: formatCurrency(row.Interest), Installment: formatCurrency(row.Installment),
				Outstanding: formatCurrency(row.ScheduledBalance), Status: displayText(paymentStatusByInstallment[int64(row.Number)]),
			})
		}
	}
	dueDates := contractualDueDates(resolved)
	installmentSummary := "-"
	if len(dueDates) > 0 {
		current := 0
		for _, dueDate := range dueDates {
			if !dueDate.After(asOf) {
				current++
			}
		}
		installmentSummary = fmt.Sprintf("%d dari %d", current, len(dueDates))
	}
	loanPeriod := "-"
	if !resolved.Position.LoanStartDate.IsZero() && len(dueDates) > 0 && resolved.Loan.TenorMonths > 0 {
		loanPeriod = fmt.Sprintf("%s | %d bln | %s", formatDate(resolved.Position.LoanStartDate), resolved.Loan.TenorMonths, formatDate(dueDates[len(dueDates)-1]))
	}
	query := url.Values{"account": {requestedAccount}, "as_of": {asOf.String()}}
	return ResultView{
		ReportingDate: asOf.String(), PeriodLabel: "Periode " + formatDate(asOf),
		CustomerName:     displayText(resolved.Loan.CustomerName),
		PrimaryAccount:   displayText(firstNonEmpty(resolved.Loan.PrimaryAccount, resolved.Position.AccountNumber)),
		AlternateAccount: displayText(resolved.Loan.AlternateAccount), Branch: displayText(firstNonEmpty(resolved.Loan.Branch, resolved.Position.Branch)),
		Product: displayText(firstNonEmpty(resolved.Loan.Product, resolved.Position.Product)), LoanPeriod: loanPeriod,
		InstallmentSummary: installmentSummary, PrincipalOutstanding: formatCurrency(resolved.Position.PrincipalOutstanding),
		PrincipalDue: formatCurrency(resolved.ActualPosition.PrincipalDue), InterestDue: formatCurrency(resolved.ActualPosition.InterestDue),
		PenaltyDue: formatCurrency(resolved.ActualPosition.PenaltyDue), ContractPrincipal: formatCurrency(resolved.Loan.PlafondLimit),
		ReferenceRate: formatRate(resolved.Loan.ReferenceRatePercent), FlatRate: formatRate(resolved.Loan.FlatRatePercent),
		Collectability: fmt.Sprint(resolved.Position.CollectabilityBI), EarlyTerminationEstimate: formatCurrency(early),
		IsReconstructed: resolved.Position.Source == loan.SourceReconstructed,
		PrintURL:        "/loans/inquiry/pdf?" + query.Encode(), ScheduleRows: schedule,
	}, nil
}

func contractualDueDates(resolved loan.ResolvedPosition) []loan.Date {
	if len(resolved.ContractualSchedule) > 0 {
		dates := make([]loan.Date, len(resolved.ContractualSchedule))
		for index, row := range resolved.ContractualSchedule {
			dates[index] = row.DueDate
		}
		return dates
	}
	dates := make([]loan.Date, len(resolved.Loan.ContractSchedule))
	for index, row := range resolved.Loan.ContractSchedule {
		dates[index] = row.DueDate
	}
	return dates
}

func formatDate(date loan.Date) string {
	if date.IsZero() {
		return "-"
	}
	return date.Time(time.UTC).Format("02 Jan 2006")
}

func formatCurrency(value loan.Money) string {
	raw := value.Format(2)
	integer, decimal, _ := strings.Cut(raw, ".")
	sign := ""
	if strings.HasPrefix(integer, "-") {
		sign, integer = "-", strings.TrimPrefix(integer, "-")
	}
	for index := len(integer) - 3; index > 0; index -= 3 {
		integer = integer[:index] + "," + integer[index:]
	}
	return "Rp " + sign + integer + "." + decimal
}

func formatRate(value loan.Money) string { return value.Format(2) + "%" }

func displayText(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "-"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
