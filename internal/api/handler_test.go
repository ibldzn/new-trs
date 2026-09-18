package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/loan"
)

type fakePositions struct {
	result  loan.ResolvedPosition
	err     error
	calls   int
	account string
	asOf    loan.Date
}

func (fake *fakePositions) GetLoanPosition(_ context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
	fake.calls++
	fake.account, fake.asOf = account, asOf
	return fake.result, fake.err
}

func testAPI(positions *fakePositions, appendAudit func(context.Context, audit.Event) error) http.Handler {
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	handler := NewHandler(positions, time.UTC, "test-secret", appendAudit, slog.New(slog.NewTextHandler(io.Discard, nil)))
	router.Route("/api/v1", handler.RegisterRoutes)
	return router
}

func requestAPI(router http.Handler, path, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestAuthentication(t *testing.T) {
	path := "/api/v1/loans/primary/contractual?as_of=2026-01-01"
	for _, test := range []struct {
		name, authorization, path string
		status                    int
	}{
		{"valid", "Bearer test-secret", path, http.StatusOK},
		{"case insensitive scheme", "bearer test-secret", path, http.StatusOK},
		{"missing", "", path, http.StatusUnauthorized},
		{"wrong", "Bearer wrong", path, http.StatusUnauthorized},
		{"basic", "Basic test-secret", path, http.StatusUnauthorized},
		{"query only", "", path + "&api_key=test-secret", http.StatusUnauthorized},
		{"tab separator", "Bearer\ttest-secret", path, http.StatusUnauthorized},
		{"leading space", " Bearer test-secret", path, http.StatusUnauthorized},
		{"double space", "Bearer  test-secret", path, http.StatusUnauthorized},
		{"trailing space", "Bearer test-secret ", path, http.StatusUnauthorized},
		{"extra token", "Bearer test-secret extra", path, http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			positions := &fakePositions{result: loan.ResolvedPosition{Loan: loan.ContractData{PrimaryAccount: "primary"}}}
			response := requestAPI(testAPI(positions, nil), test.path, test.authorization)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.status == http.StatusUnauthorized && (positions.calls != 0 || response.Body.String() != "{\"error\":\"unauthorized\"}\n") {
				t.Fatalf("unauthorized call count=%d body=%s", positions.calls, response.Body.String())
			}
		})
	}
	positions := &fakePositions{}
	router := testAPI(positions, nil)
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Add("Authorization", "Bearer test-secret")
	request.Header.Add("Authorization", "Bearer test-secret")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || positions.calls != 0 {
		t.Fatalf("duplicate Authorization headers: status=%d calls=%d", response.Code, positions.calls)
	}
}

func TestRequestValidation(t *testing.T) {
	today := loan.NewDate(time.Now(), time.UTC)
	historical := today.AddDays(-1, time.UTC)
	for _, test := range []struct {
		name, path string
		status     int
	}{
		{"missing account", "/api/v1/loans//contractual?as_of=" + historical.String(), http.StatusBadRequest},
		{"blank account", "/api/v1/loans/%20/contractual?as_of=" + historical.String(), http.StatusBadRequest},
		{"missing date", "/api/v1/loans/primary/contractual", http.StatusBadRequest},
		{"malformed date", "/api/v1/loans/primary/contractual?as_of=2026-2-1", http.StatusBadRequest},
		{"invalid date", "/api/v1/loans/primary/contractual?as_of=2026-02-30", http.StatusBadRequest},
		{"duplicate date", "/api/v1/loans/primary/contractual?as_of=" + historical.String() + "&as_of=" + today.String(), http.StatusBadRequest},
		{"future date", "/api/v1/loans/primary/contractual?as_of=" + today.AddDays(1, time.UTC).String(), http.StatusBadRequest},
		{"historical", "/api/v1/loans/primary/contractual?as_of=" + historical.String(), http.StatusOK},
		{"today", "/api/v1/loans/primary/contractual?as_of=" + today.String(), http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			positions := &fakePositions{result: loan.ResolvedPosition{Loan: loan.ContractData{PrimaryAccount: "primary"}}}
			response := requestAPI(testAPI(positions, nil), test.path, "Bearer test-secret")
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if test.status == http.StatusBadRequest && positions.calls != 0 {
				t.Fatalf("invalid request called service %d times", positions.calls)
			}
		})
	}
}

func TestContractualUsesOneResolvedPositionAndAudits(t *testing.T) {
	asOf := loan.NewDate(time.Now().AddDate(0, 0, -1), time.UTC)
	later := asOf.AddDays(1, time.UTC)
	earlier := asOf.AddDays(-2, time.UTC)
	repayments := []loan.Repayment{
		{Date: later, PrincipalComponent: loan.MustMoney("10.01"), InterestComponent: loan.MustMoney("2.02"), PenaltyComponent: loan.MustMoney("3.03"), EarlyPenaltyComponent: loan.MustMoney("4.04"), DWPComponent: loan.MustMoney("5.05"), TotalPayment: loan.MustMoney("24.15"), JournalNumber: "later", SourceOrder: 1},
		{Date: earlier, PrincipalComponent: loan.MustMoney("-1.01"), TotalPayment: loan.MustMoney("0"), JournalNumber: "earlier", SourceOrder: 0},
	}
	positions := &fakePositions{result: loan.ResolvedPosition{
		Loan:     loan.ContractData{PrimaryAccount: "primary", FlatRatePercent: loan.MustMoney("12.50"), Repayments: repayments},
		Position: loan.LoanPosition{PrincipalOutstanding: loan.MustMoney("123456789012345678901234567890.12"), Source: loan.SourceDWH},
	}}
	var events []audit.Event
	router := testAPI(positions, func(_ context.Context, event audit.Event) error {
		events = append(events, event)
		return nil
	})
	response := requestAPI(router, "/api/v1/loans/alternate/contractual?as_of="+asOf.String(), "Bearer test-secret")
	if response.Code != http.StatusOK || positions.calls != 1 || positions.account != "alternate" || !positions.asOf.Equal(asOf) {
		t.Fatalf("status=%d calls=%d account=%q asOf=%s body=%s", response.Code, positions.calls, positions.account, positions.asOf, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		`"requested_account":"alternate"`, `"primary_account":"primary"`, `"position_source":"DWH"`,
		`"contract_rate":12.50`, `"contractual_outstanding":123456789012345678901234567890.12`,
		`"principal":10.01`, `"interest":2.02`, `"penalty":3.03`,
		`"early_termination_penalty":4.04`, `"dwp":5.05`, `"total_payment":24.15`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	if strings.Contains(body, "source_order") || strings.Contains(body, `"contract_rate":"`) || strings.Contains(body, `"total_payment":"`) {
		t.Fatalf("internal field or quoted money: %s", body)
	}
	var decoded struct {
		RepaymentHistory []struct {
			PaymentDate   string `json:"payment_date"`
			JournalNumber string `json:"journal_number"`
		} `json:"repayment_history"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.RepaymentHistory) != 2 || decoded.RepaymentHistory[0].PaymentDate != earlier.String() || decoded.RepaymentHistory[0].JournalNumber != "earlier" || decoded.RepaymentHistory[1].PaymentDate != later.String() || decoded.RepaymentHistory[1].JournalNumber != "later" {
		t.Fatalf("repayment history=%+v", decoded.RepaymentHistory)
	}
	if positions.result.Loan.Repayments[0].JournalNumber != "later" {
		t.Fatal("business repayment slice mutated")
	}
	if len(events) != 1 || events[0].Action != audit.ActionAPILoanLookup || events[0].Attribution.SystemActor != "system:api" {
		t.Fatalf("audit events=%+v", events)
	}
	metadata, ok := events[0].Metadata.(audit.APILoanLookupMetadata)
	if !ok || metadata.RequestedAccount != "alternate" || metadata.PrimaryAccount != "primary" || metadata.AsOf != asOf.String() || metadata.Outcome != "success" || metadata.RequestID == "" {
		t.Fatalf("audit metadata=%+v", events[0].Metadata)
	}
	positions.calls = 0
	response = requestAPI(router, "/api/v1/loans/%20alternate%20/contractual?as_of="+asOf.String(), "Bearer test-secret")
	if response.Code != http.StatusOK || positions.calls != 1 || positions.account != "alternate" {
		t.Fatalf("trimmed account: status=%d calls=%d account=%q", response.Code, positions.calls, positions.account)
	}
}

func TestMoneyNumber(t *testing.T) {
	for _, test := range []struct{ raw, want string }{
		{"0", "0.00"}, {"1234.56", "1234.56"}, {"123456789012345678901234567890.12", "123456789012345678901234567890.12"}, {"-12.34", "-12.34"},
	} {
		encoded, err := json.Marshal(moneyNumber{loan.MustMoney(test.raw)})
		if err != nil || string(encoded) != test.want || !json.Valid(encoded) {
			t.Fatalf("%s encoded as %s: %v", test.raw, encoded, err)
		}
	}
}

func TestClosedPositionKeepsSignedRepaymentHistory(t *testing.T) {
	asOf := loan.NewDate(time.Now().AddDate(0, 0, -1), time.UTC)
	positive := loan.Repayment{Date: asOf, PrincipalComponent: loan.MustMoney("300000"), TotalPayment: loan.MustMoney("300000"), SourceOrder: 0}
	negative := loan.Repayment{Date: asOf, PrincipalComponent: loan.MustMoney("-300000"), TotalPayment: loan.MustMoney("-300000"), SourceOrder: 1}
	mixed := loan.Repayment{Date: asOf, PrincipalComponent: loan.MustMoney("336501"), InterestComponent: loan.MustMoney("-4001"), TotalPayment: loan.MustMoney("332500"), SourceOrder: 2}
	positions := &fakePositions{result: loan.ResolvedPosition{
		Loan:     loan.ContractData{PrimaryAccount: "primary", FlatRatePercent: loan.MustMoney("18"), Repayments: []loan.Repayment{negative, mixed, positive}},
		Position: loan.LoanPosition{AsOf: asOf, AccountNumber: "primary", Source: loan.SourceClosed},
	}}
	response := requestAPI(testAPI(positions, nil), "/api/v1/loans/primary/contractual?as_of="+asOf.String(), "Bearer test-secret")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{`"contract_rate":18.00`, `"contractual_outstanding":0.00`, `"position_source":"closed"`,
		`"principal":300000.00`, `"principal":-300000.00`, `"total_payment":-300000.00`,
		`"principal":336501.00`, `"interest":-4001.00`, `"total_payment":332500.00`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	if strings.Index(body, `"principal":300000.00`) >= strings.Index(body, `"principal":-300000.00`) ||
		positions.result.Loan.Repayments[0].PrincipalComponent.Cmp(loan.MustMoney("-300000")) != 0 {
		t.Fatalf("repayment history reordered or mutated: %s", body)
	}
	var decoded struct {
		RepaymentHistory []struct {
			Principal    json.Number `json:"principal"`
			Interest     json.Number `json:"interest"`
			TotalPayment json.Number `json:"total_payment"`
		} `json:"repayment_history"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.RepaymentHistory) != 3 || decoded.RepaymentHistory[2].Principal.String() != "336501.00" ||
		decoded.RepaymentHistory[2].Interest.String() != "-4001.00" || decoded.RepaymentHistory[2].TotalPayment.String() != "332500.00" {
		t.Fatalf("mixed-sign repayment history=%+v", decoded.RepaymentHistory)
	}
}

func TestPositionErrorMapping(t *testing.T) {
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{loan.ErrInvalidInput, 400, "bad_request"},
		{loan.ErrNotFound, 404, "not_found"},
		{loan.ErrAmbiguousAccountResolution, 409, "ambiguous_account"},
		{loan.ErrUnsupportedCalculation, 422, "unsupported_calculation"},
		{loan.ErrUnsupportedRepaymentReversal, 422, "unsupported_calculation"},
		{loan.ErrUnsupportedRepaymentAdjustment, 422, "unsupported_calculation"},
		{loan.ErrFincloudUnavailable, 503, "service_unavailable"},
		{loan.ErrFincloudCredentials, 503, "service_unavailable"},
		{loan.ErrFincloudSession, 503, "service_unavailable"},
		{loan.ErrHistoricalEvidence, 503, "service_unavailable"},
		{loan.ErrMSOUnavailable, 503, "service_unavailable"},
		{loan.ErrDWHUnavailable, 503, "service_unavailable"},
		{loan.ErrCurrentSnapshot, 503, "service_unavailable"},
		{errors.Join(loan.ErrNotFound, loan.ErrHistoricalEvidence), 503, "service_unavailable"},
		{errors.Join(loan.ErrNotFound, loan.ErrCurrentSnapshot), 503, "service_unavailable"},
		{loan.ErrInvariant, 500, "internal_error"},
		{errors.New("private SQL detail"), 500, "internal_error"},
	} {
		status, code := positionError(test.err)
		if status != test.status || code != test.code {
			t.Fatalf("%v mapped to %d %s, want %d %s", test.err, status, code, test.status, test.code)
		}
		positions := &fakePositions{err: test.err}
		response := requestAPI(testAPI(positions, nil), "/api/v1/loans/primary/contractual?as_of=2026-01-01", "Bearer test-secret")
		if response.Code != test.status || response.Body.String() != "{\"error\":\""+test.code+"\"}\n" || strings.Contains(response.Body.String(), "SQL") {
			t.Fatalf("HTTP status=%d body=%s", response.Code, response.Body.String())
		}
	}
}
