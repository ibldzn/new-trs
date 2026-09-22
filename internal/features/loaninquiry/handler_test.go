package loaninquiry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ibldzn/trs/internal/access"
	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/auth"
	"github.com/ibldzn/trs/internal/browserauth"
	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/loan"
	"github.com/ibldzn/trs/internal/platform/adminshell"
	"github.com/ibldzn/trs/internal/platform/navigation"
	"github.com/ibldzn/trs/internal/render"
	webfiles "github.com/ibldzn/trs/web"
)

type fakePositions struct {
	resolved loan.ResolvedPosition
	err      error
	account  string
	asOf     loan.Date
	calls    int
}

func (service *fakePositions) GetLoanPosition(_ context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
	service.account, service.asOf, service.calls = account, asOf, service.calls+1
	return service.resolved, service.err
}

type fakeAuthentication struct{ principal browserauth.Principal }

func (*fakeAuthentication) Login(context.Context, browserauth.LoginInput, time.Time) (browserauth.LoginResult, error) {
	return browserauth.LoginResult{}, browserauth.ErrInvalidCredentials
}
func (*fakeAuthentication) Labels(context.Context) (fincloud.AuthLabels, error) {
	return fincloud.AuthLabels{}, nil
}
func (service *fakeAuthentication) ResolveSession(context.Context, [32]byte, time.Time) (browserauth.Principal, error) {
	return service.principal, nil
}
func (*fakeAuthentication) Logout(context.Context, [32]byte) error { return nil }

func TestInquiryErrorDoesNotMisreportMissingEvidenceAsMissingLoan(t *testing.T) {
	status, message := inquiryError(errors.Join(loan.ErrNotFound, loan.ErrHistoricalEvidence))
	if status != http.StatusServiceUnavailable || message != "Required historical evidence is unavailable." {
		t.Fatalf("status=%d message=%q", status, message)
	}
	status, _ = inquiryError(loan.ErrNotFound)
	if status != http.StatusNotFound {
		t.Fatalf("loan not-found status=%d", status)
	}
}

func TestResultViewFormatsAdministrativeLoanPresentation(t *testing.T) {
	resolved, asOf := syntheticResolved(t, 60)
	view, err := newResultView(resolved, "0130102895", asOf)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"customer": view.CustomerName, "primary": view.PrimaryAccount, "alternate": view.AlternateAccount,
		"plafond": view.ContractPrincipal, "effective rate": view.ReferenceRate, "contract rate": view.FlatRate,
		"outstanding": view.PrincipalOutstanding, "period": view.LoanPeriod, "installments": view.InstallmentSummary,
	}
	expected := map[string]string{
		"customer": "DWI SULASTRI", "primary": "3000010000000061", "alternate": "0130102895",
		"plafond": "Rp 100,000,000.00", "effective rate": "13.80%", "contract rate": "7.80%",
		"outstanding": "Rp 43,326,865.00", "period": "30 Oct 2023 | 60 bln | 30 Oct 2028", "installments": "34 dari 60",
	}
	for field, value := range want {
		if value != expected[field] {
			t.Errorf("%s=%q want %q", field, value, expected[field])
		}
	}
	if !view.IsReconstructed || view.PeriodLabel != "Periode 18 Sep 2026" || view.Collectability != "1" {
		t.Fatalf("unexpected presentation metadata: %+v", view)
	}
	if view.PrintURL != "/loans/inquiry/pdf?account=0130102895&as_of=2026-09-18" {
		t.Fatalf("print URL=%q", view.PrintURL)
	}
}

func TestResultViewSeparatesContractualOutstandingFromActualArrears(t *testing.T) {
	resolved, asOf := syntheticResolved(t, 60)
	view, err := newResultView(resolved, resolved.Loan.PrimaryAccount, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if view.PrincipalOutstanding != "Rp 43,326,865.00" || view.PrincipalDue != "Rp 0.00" || view.InterestDue != "Rp 125,000.00" || view.PenaltyDue != "Rp 50,000.00" {
		t.Fatalf("view balances: outstanding=%q principal due=%q interest due=%q penalty due=%q", view.PrincipalOutstanding, view.PrincipalDue, view.InterestDue, view.PenaltyDue)
	}
	if resolved.Position.PrincipalDue.Format(2) != "1666666.67" || resolved.Position.InterestDue.Format(2) != "650000.00" {
		t.Fatalf("contractual reconstruction fixture lost distinction: %+v", resolved.Position)
	}
}

func TestResultViewAddsPresentationOnlyDisbursementRow(t *testing.T) {
	resolved, asOf := syntheticResolved(t, 3)
	before := append([]loan.ContractualScheduleRow(nil), resolved.ContractualSchedule...)
	view, err := newResultView(resolved, resolved.Loan.PrimaryAccount, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ScheduleRows) != 4 {
		t.Fatalf("presentation rows=%d", len(view.ScheduleRows))
	}
	row := view.ScheduleRows[0]
	if row.Number != 0 || row.Date != "30 Oct 2023" || row.Installment != "Rp 0.00" || row.Principal != "Rp 0.00" || row.Interest != "Rp 0.00" || row.Outstanding != "Rp 100,000,000.00" || row.Status != "DISBURSED" {
		t.Fatalf("row 0=%+v", row)
	}
	if len(resolved.ContractualSchedule) != len(before) {
		t.Fatalf("domain schedule length changed: %d -> %d", len(before), len(resolved.ContractualSchedule))
	}
	for index := range before {
		if resolved.ContractualSchedule[index].Number != before[index].Number || resolved.ContractualSchedule[index].DueDate != before[index].DueDate || resolved.ContractualSchedule[index].Principal.String() != before[index].Principal.String() {
			t.Fatalf("domain schedule row %d changed", index)
		}
	}
}

func TestResultViewOmitsDisbursementRowWithoutAuthoritativeStart(t *testing.T) {
	resolved, asOf := syntheticResolved(t, 3)
	resolved.Position.LoanStartDate = loan.Date{}
	view, err := newResultView(resolved, resolved.Loan.PrimaryAccount, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.ScheduleRows) != 3 || view.ScheduleRows[0].Number != 1 || view.LoanPeriod != "-" {
		t.Fatalf("unexpected presentation: period=%q rows=%+v", view.LoanPeriod, view.ScheduleRows)
	}
}

func TestCurrencyFormattingKeepsExactMoneyAndAddsGrouping(t *testing.T) {
	for _, test := range []struct {
		value string
		want  string
	}{{"100000000", "Rp 100,000,000.00"}, {"1234.5", "Rp 1,234.50"}, {"-9876543.21", "Rp -9,876,543.21"}} {
		if got := formatCurrency(loan.MustMoney(test.value)); got != test.want {
			t.Errorf("formatCurrency(%q)=%q want %q", test.value, got, test.want)
		}
	}
}

func TestSuccessfulScreenUsesCohesiveLayoutAndPreservesInquiry(t *testing.T) {
	resolved, asOf := syntheticResolved(t, 60)
	positions := &fakePositions{resolved: resolved}
	var audited audit.Event
	router, token := loanInquiryRouter(t, permittedPrincipal(), positions, func(_ context.Context, event audit.Event) error {
		audited = event
		return nil
	})
	form := url.Values{"account_number": {"0130102895"}, "as_of": {asOf.String()}}
	response := requestLoanInquiry(router, token, http.MethodPost, "/loans/inquiry", form)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, text := range []string{
		"RIWAYAT TRANSAKSI PINJAMAN", "NAMA NASABAH", "REKENING", "ALT REKENING", "KANTOR CABANG", "PRODUK", "PERIODE PINJAMAN", "ANGSURAN", "DENDA PELUNASAN DIPERCEPAT",
		"PLAFON AKAD", "SB EFEKTIF", "SB KONTRAK", "KOLEK", "BAKI DEBET", "TUNGGAKAN POKOK", "TUNGGAKAN BUNGA", "TUNGGAKAN PINALTI",
		"REPAYMENT PLAN", "Installment Schedule", "Print PDF", "NO.", "DATE", "INSTALLMENT", "PRINCIPAL", "INTEREST", "OUTSTANDING", "STATUS",
		"DWI SULASTRI", "Rp 100,000,000.00", "Rp 43,326,865.00", "Rp 125,000.00", "Rp 50,000.00", "34 dari 60", "DISBURSED",
	} {
		if !strings.Contains(body, text) {
			t.Errorf("screen missing %q", text)
		}
	}
	for _, text := range []string{"Principal Paid", "Interest Paid", "Periodic Rp", ">ET estimate<"} {
		if strings.Contains(body, text) {
			t.Errorf("screen retains old dashboard content %q", text)
		}
	}
	if !strings.Contains(body, `value="0130102895"`) || !strings.Contains(body, `value="2026-09-18"`) || !strings.Contains(body, `href="/loans/inquiry/pdf?account=0130102895&amp;as_of=2026-09-18" target="_blank" rel="noopener"`) {
		t.Fatalf("inquiry values or exact PDF URL missing")
	}
	if positions.calls != 1 || positions.account != "0130102895" || !positions.asOf.Equal(asOf) {
		t.Fatalf("position call=%+v", positions)
	}
	if audited.Action != audit.ActionLoanInquiry {
		t.Fatalf("audit event=%+v", audited)
	}
}

func TestFailedInquiryKeepsSelectedAccountAndReportingDate(t *testing.T) {
	positions := &fakePositions{err: loan.ErrNotFound}
	router, token := loanInquiryRouter(t, permittedPrincipal(), positions, nil)
	form := url.Values{"account_number": {"0130102895"}, "as_of": {"2026-09-18"}}
	response := requestLoanInquiry(router, token, http.MethodPost, "/loans/inquiry", form)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `value="0130102895"`) || !strings.Contains(response.Body.String(), `value="2026-09-18"`) {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestPDFRouteRequiresInquiryPermission(t *testing.T) {
	resolved, _ := syntheticResolved(t, 60)
	positions := &fakePositions{resolved: resolved}
	principal := permittedPrincipal()
	principal.Permissions = access.NewPermissionSet(nil)
	router, token := loanInquiryRouter(t, principal, positions, nil)
	response := requestLoanInquiry(router, token, http.MethodGet, "/loans/inquiry/pdf?account=0130102895&as_of=2026-09-18", nil)
	if response.Code != http.StatusForbidden || positions.calls != 0 {
		t.Fatalf("status=%d calls=%d", response.Code, positions.calls)
	}
}

func TestPDFRejectsMissingOrMalformedInput(t *testing.T) {
	positions := &fakePositions{}
	router, token := loanInquiryRouter(t, permittedPrincipal(), positions, nil)
	for _, path := range []string{
		"/loans/inquiry/pdf", "/loans/inquiry/pdf?account=0130102895", "/loans/inquiry/pdf?as_of=2026-09-18",
		"/loans/inquiry/pdf?account=0130102895&as_of=bad", "/loans/inquiry/pdf?account=a&account=b&as_of=2026-09-18",
	} {
		response := requestLoanInquiry(router, token, http.MethodGet, path, nil)
		if response.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s status=%d body=%q", path, response.Code, response.Body.String())
		}
	}
	if positions.calls != 0 {
		t.Fatalf("invalid requests reached position service %d times", positions.calls)
	}
}

func TestPDFUsesSharedPositionAndReturnsBrandedMultipageDocument(t *testing.T) {
	resolved, asOf := syntheticResolved(t, 60)
	positions := &fakePositions{resolved: resolved}
	router, token := loanInquiryRouter(t, permittedPrincipal(), positions, nil)
	response := requestLoanInquiry(router, token, http.MethodGet, "/loans/inquiry/pdf?account=0130102895&as_of=2026-09-18", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if positions.calls != 1 || positions.account != "0130102895" || !positions.asOf.Equal(asOf) {
		t.Fatalf("position call=%+v", positions)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/pdf" {
		t.Fatalf("content type=%q", contentType)
	}
	if disposition := response.Header().Get("Content-Disposition"); disposition != `inline; filename="jadwal-pembayaran-0130102895-2026-09-18.pdf"` {
		t.Fatalf("content disposition=%q", disposition)
	}
	document := response.Body.Bytes()
	if len(document) < 1000 || !strings.HasPrefix(string(document), "%PDF-") {
		t.Fatalf("invalid PDF length=%d prefix=%q", len(document), document[:min(5, len(document))])
	}
	if len(bankLogo) == 0 || !strings.Contains(string(document), "/Subtype /Image") {
		t.Fatal("embedded Bank DP Taspen logo image is missing")
	}
	if pages := len(regexp.MustCompile(`/Type /Page\b`).FindAll(document, -1)); pages < 2 {
		t.Fatalf("60-installment PDF page count=%d", pages)
	}
	for _, value := range []string{"DWI SULASTRI", "3000010000000061", "Rp 100,000,000.00", "Rp 43,326,865.00", "Rp 125,000.00", "Rp 50,000.00", "34 dari 60", "DISBURSED"} {
		if !strings.Contains(string(document), value) {
			t.Errorf("PDF missing shared presentation value %q", value)
		}
	}
}

func TestPDFNotFoundIsAnHTTPErrorNotAPDF(t *testing.T) {
	positions := &fakePositions{err: loan.ErrNotFound}
	router, token := loanInquiryRouter(t, permittedPrincipal(), positions, nil)
	response := requestLoanInquiry(router, token, http.MethodGet, "/loans/inquiry/pdf?account=missing&as_of=2026-09-18", nil)
	if response.Code != http.StatusNotFound || response.Header().Get("Content-Type") == "application/pdf" || strings.HasPrefix(response.Body.String(), "%PDF-") {
		t.Fatalf("status=%d type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
}

func TestSafePDFFilenamePart(t *testing.T) {
	if got := safeFilenamePart(" ../../013 01\r\n.pdf "); got != "013-01-pdf" {
		t.Fatalf("safe filename=%q", got)
	}
}

func syntheticResolved(t *testing.T, rowCount int) (loan.ResolvedPosition, loan.Date) {
	t.Helper()
	location := time.FixedZone("Jakarta", 7*60*60)
	start, err := loan.ParseDate("2023-10-30", location)
	if err != nil {
		t.Fatal(err)
	}
	asOf, err := loan.ParseDate("2026-09-18", location)
	if err != nil {
		t.Fatal(err)
	}
	contract := make([]loan.ContractualInstallment, 0, rowCount)
	schedule := make([]loan.ContractualScheduleRow, 0, rowCount)
	for number := 1; number <= rowCount; number++ {
		dueDate := loan.NewDate(start.Time(location).AddDate(0, number, 0), location)
		contract = append(contract, loan.ContractualInstallment{Number: number, DueDate: dueDate})
		schedule = append(schedule, loan.ContractualScheduleRow{
			Number: number, DueDate: dueDate, Principal: loan.MustMoney("1666666.67"), Interest: loan.MustMoney("650000"),
			Installment: loan.MustMoney("2316666.67"), ScheduledBalance: loan.MoneyFromInt(int64(100000000 - number*1000000)),
		})
	}
	return loan.ResolvedPosition{
		Loan: loan.ContractData{
			PrimaryAccount: "3000010000000061", AlternateAccount: "0130102895", CustomerName: "DWI SULASTRI",
			Branch: "KANTOR CABANG JAKARTA", Product: "KREDIT PENSIUN", PlafondLimit: loan.MustMoney("100000000"),
			TenorMonths: rowCount, ReferenceRatePercent: loan.MustMoney("13.80"), FlatRatePercent: loan.MustMoney("7.80"),
			PenaltyDue: loan.MustMoney("125000"), ContractSchedule: contract,
		},
		Position: loan.LoanPosition{
			AsOf: asOf, LoanStartDate: start, AccountNumber: "3000010000000061", PrincipalOutstanding: loan.MustMoney("43326865"),
			PrincipalDue: loan.MustMoney("1666666.67"), InterestDue: loan.MustMoney("650000"), CollectabilityBI: 1, Source: loan.SourceReconstructed,
		},
		ActualPosition: loan.LoanPosition{
			AsOf: asOf, LoanStartDate: start, AccountNumber: "3000010000000061", PrincipalOutstanding: loan.MustMoney("43326865"),
			PrincipalDue: loan.MustMoney("0"), InterestDue: loan.MustMoney("125000"), PenaltyDue: loan.MustMoney("50000"), CollectabilityBI: 1, Source: loan.SourceDWH,
		},
		ContractualSchedule: schedule,
	}, asOf
}

func permittedPrincipal() browserauth.Principal {
	return browserauth.Principal{
		UserID: 1, Username: "viewer",
		Permissions: access.NewPermissionSet([]string{PermissionInquiry}),
	}
}

func loanInquiryRouter(t *testing.T, principal browserauth.Principal, positions positionService, appendAudit func(context.Context, audit.Event) error) (http.Handler, string) {
	t.Helper()
	renderer, err := render.New(webfiles.Files, false)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	errors := render.NewErrorResponder(renderer, "Test", logger)
	registry, err := navigation.NewRegistry([]navigation.Group{{Key: "general", Label: "General", Items: []navigation.Item{Navigation()}}}, PermissionDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	cookies := browserauth.NewCookieManager("session", false, time.Hour)
	authentication := browserauth.NewHTTP(&fakeAuthentication{principal}, renderer, cookies, "Test", logger, func(context.Context, audit.Event) error { return nil }, errors)
	router := chi.NewRouter()
	router.Use(authentication.LoadPrincipal)
	NewHandler(adminshell.New(renderer, registry, "Test", errors), positions, time.FixedZone("Jakarta", 7*60*60), appendAudit, logger).RegisterRoutes(router)
	token, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	return router, token
}

func requestLoanInquiry(router http.Handler, token, method, path string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, path, body)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	request.AddCookie(&http.Cookie{Name: "session", Value: token})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
