package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/loan"
)

type positionService interface {
	GetLoanPosition(context.Context, string, loan.Date) (loan.ResolvedPosition, error)
}

type Handler struct {
	positions   positionService
	location    *time.Location
	keyHash     [sha256.Size]byte
	appendAudit func(context.Context, audit.Event) error
	logger      *slog.Logger
}

func NewHandler(positions positionService, location *time.Location, key string, appendAudit func(context.Context, audit.Event) error, logger *slog.Logger) *Handler {
	return &Handler{positions: positions, location: location, keyHash: sha256.Sum256([]byte(key)), appendAudit: appendAudit, logger: logger}
}

func (handler *Handler) RegisterRoutes(router chi.Router) {
	router.Use(handler.authenticate)
	router.Get("/loans/{account}/contractual", handler.contractual)
	router.Get("/loans//contractual", handler.contractual)
}

func (handler *Handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		values := request.Header.Values("Authorization")
		if len(values) != 1 {
			writeError(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		scheme, key, found := strings.Cut(values[0], " ")
		if !found || !strings.EqualFold(scheme, "Bearer") || key == "" || strings.ContainsAny(key, " \t\r\n") {
			writeError(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		supplied := sha256.Sum256([]byte(key))
		if subtle.ConstantTimeCompare(supplied[:], handler.keyHash[:]) != 1 {
			writeError(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(writer, request)
	})
}

type moneyNumber struct{ value loan.Money }

func (number moneyNumber) MarshalJSON() ([]byte, error) {
	return []byte(number.value.Format(2)), nil
}

type repaymentResponse struct {
	PaymentDate             string      `json:"payment_date"`
	Principal               moneyNumber `json:"principal"`
	Interest                moneyNumber `json:"interest"`
	Penalty                 moneyNumber `json:"penalty"`
	EarlyTerminationPenalty moneyNumber `json:"early_termination_penalty"`
	DWP                     moneyNumber `json:"dwp"`
	TotalPayment            moneyNumber `json:"total_payment"`
	JournalNumber           string      `json:"journal_number"`
}

type contractualResponse struct {
	RequestedAccount       string              `json:"requested_account"`
	PrimaryAccount         string              `json:"primary_account"`
	AsOf                   string              `json:"as_of"`
	ContractRate           moneyNumber         `json:"contract_rate"`
	ContractualOutstanding moneyNumber         `json:"contractual_outstanding"`
	PositionSource         loan.PositionSource `json:"position_source"`
	RepaymentHistory       []repaymentResponse `json:"repayment_history"`
}

func (handler *Handler) contractual(writer http.ResponseWriter, request *http.Request) {
	account := strings.TrimSpace(chi.URLParam(request, "account"))
	var asOf loan.Date
	primary, outcome := "", "bad_request"
	defer func() { handler.audit(request, account, primary, asOf, outcome) }()

	dates := request.URL.Query()["as_of"]
	if account == "" || len(dates) != 1 {
		writeError(writer, http.StatusBadRequest, "bad_request")
		return
	}
	parsed, err := loan.ParseDate(dates[0], handler.location)
	if err != nil || parsed.String() != dates[0] || len(dates[0]) != len(loan.DateLayout) {
		writeError(writer, http.StatusBadRequest, "bad_request")
		return
	}
	asOf = parsed
	if asOf.After(loan.NewDate(time.Now(), handler.location)) {
		writeError(writer, http.StatusBadRequest, "bad_request")
		return
	}

	resolved, err := handler.positions.GetLoanPosition(request.Context(), account, asOf)
	if err != nil {
		status, code := positionError(err)
		outcome = code
		if handler.logger != nil && status >= http.StatusInternalServerError {
			handler.logger.ErrorContext(request.Context(), "API loan lookup failed", "error", err, "request_id", middleware.GetReqID(request.Context()))
		}
		writeError(writer, status, code)
		return
	}
	primary = resolved.Loan.PrimaryAccount
	outcome = "success"
	repayments := slices.Clone(resolved.Loan.Repayments)
	slices.SortStableFunc(repayments, func(left, right loan.Repayment) int {
		if left.Date.Before(right.Date) {
			return -1
		}
		if left.Date.After(right.Date) {
			return 1
		}
		return cmp.Compare(left.SourceOrder, right.SourceOrder)
	})
	response := contractualResponse{
		RequestedAccount: account, PrimaryAccount: primary, AsOf: asOf.String(),
		ContractRate: moneyNumber{resolved.Loan.FlatRatePercent}, ContractualOutstanding: moneyNumber{resolved.Position.PrincipalOutstanding},
		PositionSource: resolved.Position.Source, RepaymentHistory: make([]repaymentResponse, 0, len(repayments)),
	}
	for _, payment := range repayments {
		response.RepaymentHistory = append(response.RepaymentHistory, repaymentResponse{
			PaymentDate: payment.Date.String(), Principal: moneyNumber{payment.PrincipalComponent}, Interest: moneyNumber{payment.InterestComponent},
			Penalty: moneyNumber{payment.PenaltyComponent}, EarlyTerminationPenalty: moneyNumber{payment.EarlyPenaltyComponent},
			DWP: moneyNumber{payment.DWPComponent}, TotalPayment: moneyNumber{payment.TotalPayment}, JournalNumber: payment.JournalNumber,
		})
	}
	writeJSON(writer, http.StatusOK, response)
}

func (handler *Handler) audit(request *http.Request, account, primary string, asOf loan.Date, outcome string) {
	if handler.appendAudit == nil {
		return
	}
	event := audit.Event{
		Attribution: audit.Attribution{SystemActor: "system:api"}, Action: audit.ActionAPILoanLookup,
		Metadata: audit.APILoanLookupMetadata{
			RequestedAccount: account, PrimaryAccount: primary, AsOf: asOf.String(),
			Outcome: outcome, RequestID: middleware.GetReqID(request.Context()),
		},
		CreatedAt: time.Now().UTC(),
	}
	if err := handler.appendAudit(request.Context(), event); err != nil && handler.logger != nil {
		handler.logger.WarnContext(request.Context(), "append API loan lookup audit", "error", err)
	}
}

func positionError(err error) (int, string) {
	switch {
	case errors.Is(err, loan.ErrInvalidInput):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, loan.ErrAmbiguousAccountResolution):
		return http.StatusConflict, "ambiguous_account"
	case errors.Is(err, loan.ErrUnsupportedCalculation):
		return http.StatusUnprocessableEntity, "unsupported_calculation"
	case errors.Is(err, loan.ErrHistoricalEvidence), errors.Is(err, loan.ErrCurrentSnapshot),
		errors.Is(err, loan.ErrFincloudCredentials), errors.Is(err, loan.ErrFincloudSession),
		errors.Is(err, loan.ErrFincloudUnavailable), errors.Is(err, loan.ErrDWHUnavailable),
		errors.Is(err, loan.ErrMSOUnavailable):
		return http.StatusServiceUnavailable, "service_unavailable"
	case errors.Is(err, loan.ErrNotFound):
		return http.StatusNotFound, "not_found"
	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

func writeError(writer http.ResponseWriter, status int, code string) {
	writeJSON(writer, status, struct {
		Error string `json:"error"`
	}{code})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
