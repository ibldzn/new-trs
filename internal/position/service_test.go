package position

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/loan"
)

type fincloudFake struct {
	contract loan.ContractData
	err      error
	calls    int
}

func (fake *fincloudFake) ResolveLoan(context.Context, string, *time.Location) (loan.ContractData, error) {
	fake.calls++
	return fake.contract, fake.err
}

func TestPositionServiceClosedAsOfSkipsHistoricalDependencies(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	for _, test := range []struct{ name, closeDate, asOf string }{
		{"closed before as-of", "2026-08-20", "2026-08-31"},
		{"closed on as-of", "2026-08-31", "2026-08-31"},
		{"closed today", "2026-09-03", "2026-09-14"},
		{"closed before cutoff", "2025-10-11", "2025-10-11"},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract := loan.ContractData{
				PrimaryAccount: "primary", FlatRatePercent: loan.MustMoney("18"),
				CloseDate: parseDate(test.closeDate, location), CurrentCollectability: 5, Status: "Closed",
				ContractScheduleEvidence: []loan.ContractualInstallmentEvidence{{Number: 1, RawDueDate: "invalid"}},
				Repayments:               []loan.Repayment{{PrincipalComponent: loan.MustMoney("-300000"), TotalPayment: loan.MustMoney("-300000")}},
			}
			fincloud := &fincloudFake{contract: contract}
			mso, dwh, snapshot, calculator := &msoFake{}, &dwhFake{}, &snapshotFake{}, &calculatorFake{}
			service, _ := NewService(fincloud, mso, dwh, snapshot, calculator, location)
			service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }
			asOf := parseDate(test.asOf, location)
			resolved, err := service.GetLoanPosition(context.Background(), "input", asOf)
			if err != nil {
				t.Fatal(err)
			}
			position := resolved.Position
			if !reflect.DeepEqual(resolved.Loan, contract) || position.Source != loan.SourceClosed || !position.AsOf.Equal(asOf) ||
				position.AccountNumber != "primary" || position.CollectabilityBI != 5 ||
				!position.PrincipalOutstanding.IsZero() || !position.PrincipalDue.IsZero() || !position.InterestDue.IsZero() ||
				!resolved.ActualPosition.PrincipalDue.IsZero() || !resolved.ActualPosition.InterestDue.IsZero() || !resolved.ActualPosition.PenaltyDue.IsZero() ||
				fincloud.calls != 1 || mso.historicalCalls != 0 || mso.openingCalls != 0 ||
				dwh.exactCalls != 0 || dwh.timelineCalls != 0 || snapshot.calls != 0 || calculator.calls != 0 {
				t.Fatalf("resolved=%+v calls: Fincloud=%d MSO=%d/%d DWH=%d/%d snapshot=%d calculator=%d",
					resolved, fincloud.calls, mso.historicalCalls, mso.openingCalls, dwh.exactCalls, dwh.timelineCalls, snapshot.calls, calculator.calls)
			}
		})
	}
}

func TestPositionServiceClosedAlternateFromFincloudDateObject(t *testing.T) {
	const alternate, primary = "0123456789", "3000000000000001"
	var detailCalls, searchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/admin/access/login":
			_, _ = writer.Write([]byte(`{"status":"ok","data":{"result":{"sessionid":"session"}}}`))
		case "/pinjaman/inquiry/rekening/pinjaman":
			detailCalls.Add(1)
			switch request.URL.Query().Get("id") {
			case alternate:
				_, _ = writer.Write([]byte(`{"status":"ok","data":{"result":{}}}`))
			case primary:
				_, _ = writer.Write([]byte(`{"status":"ok","data":{"result":{"id":"3000000000000001","noalt":"0123456789","plafondlimit":"100","jangkawaktu":"1 bulan","bungaflat":"11.76","kolekbi":2,"statusrekening":"Closed","tgl_tutup":{"date":"2026-08-20 00:00:00.000000","timezone_type":3,"timezone":"Asia/Jakarta"}}}}`))
			default:
				http.NotFound(writer, request)
			}
		case "/pinjaman/inquiry/rekening/cari":
			searchCalls.Add(1)
			if request.URL.Query().Get("cabang") != "ALL" || request.URL.Query().Get("noalt") != alternate || request.URL.Query().Get("pagesize") != "50" {
				t.Errorf("unexpected alternate search query: %s", request.URL.RawQuery)
			}
			_, _ = writer.Write([]byte(`{"status":"ok","data":{"result":[{"id":"3000000000000001","noalt":""}]}}`))
		case "/admin/access/logout":
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := fincloud.NewClient(fincloud.Config{BaseURL: server.URL, Username: "system", Password: "secret", LocationID: "000", RoleID: "R-1", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())

	location := time.FixedZone("Jakarta", 7*60*60)
	mso, dwh, snapshot, calculator := &msoFake{}, &dwhFake{}, &snapshotFake{}, &calculatorFake{}
	service, err := NewService(client, mso, dwh, snapshot, calculator, location)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }
	resolved, err := service.GetLoanPosition(context.Background(), alternate, parseDate("2026-08-31", location))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Loan.PrimaryAccount != primary || resolved.Loan.AlternateAccount != alternate || resolved.Loan.CloseDate.String() != "2026-08-20" ||
		resolved.Loan.FlatRatePercent.Cmp(loan.MustMoney("11.76")) != 0 || resolved.Position.Source != loan.SourceClosed ||
		resolved.Position.AccountNumber != primary || resolved.Position.CollectabilityBI != 2 ||
		!resolved.Position.PrincipalOutstanding.IsZero() || !resolved.Position.PrincipalDue.IsZero() || !resolved.Position.InterestDue.IsZero() ||
		!resolved.ActualPosition.PrincipalDue.IsZero() || !resolved.ActualPosition.InterestDue.IsZero() || !resolved.ActualPosition.PenaltyDue.IsZero() ||
		detailCalls.Load() != 2 || searchCalls.Load() != 1 || mso.historicalCalls != 0 || mso.openingCalls != 0 ||
		dwh.exactCalls != 0 || dwh.timelineCalls != 0 || snapshot.calls != 0 || calculator.calls != 0 {
		t.Fatalf("resolved=%+v calls: detail=%d search=%d MSO=%d/%d DWH=%d/%d snapshot=%d calculator=%d",
			resolved, detailCalls.Load(), searchCalls.Load(), mso.historicalCalls, mso.openingCalls, dwh.exactCalls, dwh.timelineCalls, snapshot.calls, calculator.calls)
	}
}

func TestPositionServiceFutureOrAbsentCloseUsesHistoricalCalculation(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	for _, test := range []struct{ name, closeDate string }{
		{"closure after as-of", "2026-09-03"},
		{"no closure", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract := loan.ContractData{PrimaryAccount: "primary", AlternateAccount: "0130112345", FlatRatePercent: loan.MustMoney("18"),
				PlafondLimit: loan.MustMoney("100"), TenorMonths: 1,
				ContractSchedule: []loan.ContractualInstallment{{Number: 1, DueDate: parseDate("2025-11-12", location)}}}
			if test.closeDate != "" {
				contract.CloseDate = parseDate(test.closeDate, location)
			}
			mso := &msoFake{opening: loan.OpeningLoanState{InterestType: FlatInterestType, PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 1}}
			dwh, snapshot := &dwhFake{exact: loan.LoanPosition{LoanStartDate: parseDate("2024-06-01", location)}}, &snapshotFake{}
			calculator := &calculatorFake{result: loan.CalculationResult{PrincipalOutstanding: loan.MustMoney("55"), CollectabilityBI: 1}}
			service, _ := NewService(&fincloudFake{contract: contract}, mso, dwh, snapshot, calculator, location)
			service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }
			resolved, err := service.GetLoanPosition(context.Background(), "input", parseDate("2026-08-31", location))
			if err != nil || resolved.Position.Source != loan.SourceReconstructed || resolved.Position.PrincipalOutstanding.Cmp(loan.MustMoney("55")) != 0 ||
				mso.openingCalls != 1 || dwh.timelineCalls != 1 || calculator.calls != 1 {
				t.Fatalf("resolved=%+v err=%v calls: MSO=%d DWH=%d calculator=%d", resolved, err, mso.openingCalls, dwh.timelineCalls, calculator.calls)
			}
		})
	}
}

func TestPositionServiceNotFoundRemainsError(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	fincloud := &fincloudFake{err: loan.ErrNotFound}
	mso, dwh, snapshot, calculator := &msoFake{}, &dwhFake{}, &snapshotFake{}, &calculatorFake{}
	service, _ := NewService(fincloud, mso, dwh, snapshot, calculator, location)
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }
	_, err := service.GetLoanPosition(context.Background(), "missing", parseDate("2026-08-31", location))
	if !errors.Is(err, loan.ErrNotFound) || fincloud.calls != 1 || mso.openingCalls != 0 || dwh.timelineCalls != 0 || snapshot.calls != 0 || calculator.calls != 0 {
		t.Fatalf("error=%v calls: Fincloud=%d MSO=%d DWH=%d snapshot=%d calculator=%d", err, fincloud.calls, mso.openingCalls, dwh.timelineCalls, snapshot.calls, calculator.calls)
	}
}

type msoFake struct {
	historical        loan.LoanPosition
	opening           loan.OpeningLoanState
	openingError      error
	historicalCalls   int
	openingCalls      int
	historicalAccount string
	openingAccount    string
}

func (fake *msoFake) HistoricalPosition(_ context.Context, account string, _ loan.Date) (loan.LoanPosition, error) {
	fake.historicalCalls++
	fake.historicalAccount = account
	return fake.historical, nil
}
func (fake *msoFake) OpeningState(_ context.Context, account string, _ loan.Date) (loan.OpeningLoanState, error) {
	fake.openingCalls++
	fake.openingAccount = account
	return fake.opening, fake.openingError
}

type dwhFake struct {
	exact           loan.LoanPosition
	exactError      error
	timeline        []loan.CollectabilityPoint
	timelineError   error
	exactCalls      int
	timelineCalls   int
	exactAccount    string
	timelineAccount string
}

func (fake *dwhFake) ExactPosition(_ context.Context, account string, _ loan.Date) (loan.LoanPosition, error) {
	fake.exactCalls++
	fake.exactAccount = account
	return fake.exact, fake.exactError
}
func (fake *dwhFake) CollectabilityTimeline(_ context.Context, account string, _, _ loan.Date) ([]loan.CollectabilityPoint, error) {
	fake.timelineCalls++
	fake.timelineAccount = account
	return fake.timeline, fake.timelineError
}

type snapshotFake struct {
	position loan.LoanPosition
	err      error
	calls    int
	account  string
}

func (fake *snapshotFake) ExactPosition(_ context.Context, account string, _ loan.Date) (loan.LoanPosition, error) {
	fake.calls++
	fake.account = account
	return fake.position, fake.err
}

type calculatorFake struct {
	input  loan.CalculationInput
	result loan.CalculationResult
	calls  int
}

func (fake *calculatorFake) Calculate(input loan.CalculationInput) (loan.CalculationResult, error) {
	fake.calls++
	fake.input = input
	return fake.result, nil
}

func TestPositionServiceSelectsAuthoritativeSource(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	today := parseDate("2026-09-14", location)
	for _, test := range []struct {
		name                                                                                        string
		asOf                                                                                        string
		loanStartDate                                                                               string
		interestType                                                                                string
		unusableSchedule                                                                            bool
		wantSource                                                                                  loan.PositionSource
		wantMSOHistorical, wantMSOOpening, wantDWHExact, wantTimeline, wantSnapshot, wantCalculator int
	}{
		{name: "pre-cutoff uses MSO", asOf: "2025-10-11", interestType: "10", wantSource: loan.SourceMSO, wantMSOHistorical: 1},
		{name: "cutoff uses MSO opening without exact evidence", asOf: "2025-10-12", interestType: "10", wantSource: loan.SourceMSO, wantMSOOpening: 1},
		{name: "start exactly on cutoff stays migrated", asOf: "2026-09-13", loanStartDate: "2025-10-12", interestType: "20", wantSource: loan.SourceDWH, wantMSOOpening: 1, wantDWHExact: 1},
		{name: "non-contractual historical ignores unusable flat schedule", asOf: "2026-09-13", interestType: "20", unusableSchedule: true, wantSource: loan.SourceDWH, wantMSOOpening: 1, wantDWHExact: 1},
		{name: "non-contractual today uses current snapshot", asOf: "2026-09-14", interestType: "20", wantSource: loan.SourceTodaySnapshot, wantMSOOpening: 1, wantSnapshot: 1},
		{name: "reconstructed post-cutoff loan", asOf: "2026-09-13", interestType: "10", wantSource: loan.SourceReconstructed, wantMSOOpening: 1, wantDWHExact: 1, wantTimeline: 1, wantCalculator: 1},
		{name: "supported today requires snapshot collectability", asOf: "2026-09-14", interestType: "10", wantSource: loan.SourceReconstructed, wantMSOOpening: 1, wantTimeline: 1, wantSnapshot: 1, wantCalculator: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			asOf := parseDate(test.asOf, location)
			loanStartDate := test.loanStartDate
			if loanStartDate == "" {
				loanStartDate = "2024-06-01"
			}
			contract := loan.ContractData{PrimaryAccount: "3080020000000094", AlternateAccount: "0130112345", PlafondLimit: loan.MustMoney("100"), TenorMonths: 1, FlatRatePercent: loan.MustMoney("12"), ContractSchedule: []loan.ContractualInstallment{{Number: 1, DueDate: parseDate("2025-11-12", location)}}}
			if test.unusableSchedule {
				contract.ContractSchedule = nil
				contract.ContractScheduleEvidence = []loan.ContractualInstallmentEvidence{{Number: 1, RawDueDate: "bad"}}
			}
			fincloud := &fincloudFake{contract: contract}
			mso := &msoFake{
				historical: loan.LoanPosition{AccountNumber: "01.301.12345", PrincipalDue: loan.MustMoney("10"), InterestDue: loan.MustMoney("5"), PenaltyDue: loan.MustMoney("2"), Source: loan.SourceMSO},
				opening:    loan.OpeningLoanState{AccountNumber: "01.301.12345", InterestType: test.interestType, PrincipalOutstanding: loan.MustMoney("100"), PrincipalDue: loan.MustMoney("11"), InterestDue: loan.MustMoney("6"), PenaltyDue: loan.MustMoney("3"), CollectabilityBI: 2},
			}
			dwh := &dwhFake{exact: loan.LoanPosition{LoanStartDate: parseDate(loanStartDate, location), PrincipalDue: loan.MustMoney("20"), InterestDue: loan.MustMoney("7"), PenaltyDue: loan.MustMoney("4"), Source: loan.SourceDWH}, timeline: []loan.CollectabilityPoint{{Date: parseDate("2026-09-13", location), Value: 3}}}
			snapshot := &snapshotFake{position: loan.LoanPosition{LoanStartDate: parseDate(loanStartDate, location), PrincipalDue: loan.MustMoney("30"), InterestDue: loan.MustMoney("8"), PenaltyDue: loan.MustMoney("5"), Source: loan.SourceTodaySnapshot, CollectabilityBI: 4}}
			calculator := &calculatorFake{result: loan.CalculationResult{AsOf: asOf, PrincipalOutstanding: loan.MustMoney("55"), PrincipalDue: loan.MustMoney("40"), InterestDue: loan.MustMoney("9"), CollectabilityBI: 4}}
			service, err := NewService(fincloud, mso, dwh, snapshot, calculator, location)
			if err != nil {
				t.Fatal(err)
			}
			service.now = func() time.Time { return today.Time(location).Add(12 * time.Hour) }
			result, err := service.GetLoanPosition(context.Background(), "input", asOf)
			if err != nil {
				t.Fatal(err)
			}
			if result.Position.Source != test.wantSource || mso.historicalCalls != test.wantMSOHistorical || mso.openingCalls != test.wantMSOOpening || dwh.exactCalls != test.wantDWHExact || dwh.timelineCalls != test.wantTimeline || snapshot.calls != test.wantSnapshot || calculator.calls != test.wantCalculator {
				t.Fatalf("source=%s calls: mso-history=%d opening=%d dwh=%d timeline=%d snapshot=%d calculator=%d", result.Position.Source, mso.historicalCalls, mso.openingCalls, dwh.exactCalls, dwh.timelineCalls, snapshot.calls, calculator.calls)
			}
			if test.wantMSOHistorical == 1 && mso.historicalAccount != "01.301.12345" {
				t.Fatalf("MSO historical account = %q", mso.historicalAccount)
			}
			if test.wantMSOOpening == 1 && mso.openingAccount != "01.301.12345" {
				t.Fatalf("MSO opening account = %q", mso.openingAccount)
			}
			if test.wantDWHExact == 1 && dwh.exactAccount != contract.PrimaryAccount {
				t.Fatalf("DWH exact account = %q", dwh.exactAccount)
			}
			if test.wantTimeline == 1 && dwh.timelineAccount != contract.PrimaryAccount {
				t.Fatalf("DWH timeline account = %q", dwh.timelineAccount)
			}
			if test.wantSnapshot == 1 && snapshot.account != contract.PrimaryAccount {
				t.Fatalf("snapshot account = %q", snapshot.account)
			}
			if test.wantSource == loan.SourceMSO && result.Position.AccountNumber != contract.PrimaryAccount {
				t.Fatalf("canonical result account = %q", result.Position.AccountNumber)
			}
			if test.wantCalculator == 1 {
				wantActual := dwh.exact
				if asOf.Equal(today) {
					wantActual = snapshot.position
				}
				if result.Position.PrincipalOutstanding.Cmp(loan.MustMoney("55")) != 0 || result.Position.PrincipalDue.Cmp(loan.MustMoney("40")) != 0 || result.Position.InterestDue.Cmp(loan.MustMoney("9")) != 0 || !reflect.DeepEqual(result.ActualPosition, wantActual) {
					t.Fatalf("contractual=%+v actual=%+v want actual=%+v", result.Position, result.ActualPosition, wantActual)
				}
				if calculator.input.Opening.CollectabilityBI != 2 || len(calculator.input.CollectabilityTimeline) != test.wantTimeline+test.wantSnapshot || calculator.input.ContractualPrincipal.Format(2) != "100.00" {
					t.Fatalf("calculator input = %+v", calculator.input)
				}
				if asOf.Equal(today) && calculator.input.CollectabilityTimeline[len(calculator.input.CollectabilityTimeline)-1].Value != 4 {
					t.Fatalf("today collectability was not reused from snapshot: %+v", calculator.input.CollectabilityTimeline)
				}
			} else if !reflect.DeepEqual(result.ActualPosition, result.Position) {
				t.Fatalf("selected=%+v actual=%+v", result.Position, result.ActualPosition)
			}
		})
	}
}

func TestPositionServiceFincloudNativeRoutesWithoutMSO(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	for _, test := range []struct {
		name, loanStartDate, closeDate, asOf string
		wantSource                           loan.PositionSource
		wantDWH, wantSnapshot                int
		wantError                            error
	}{
		{name: "historical without alternate", loanStartDate: "2026-01-15", asOf: "2026-08-31", wantSource: loan.SourceDWH, wantDWH: 1},
		{name: "today without alternate", loanStartDate: "2026-01-15", asOf: "2026-09-14", wantSource: loan.SourceTodaySnapshot, wantSnapshot: 1},
		{name: "day after cutoff", loanStartDate: "2025-10-13", asOf: "2025-10-13", wantSource: loan.SourceDWH, wantDWH: 1},
		{name: "closed native loan", loanStartDate: "2026-01-15", closeDate: "2026-08-20", asOf: "2026-08-31", wantSource: loan.SourceClosed},
		{name: "start date after as-of", loanStartDate: "2026-01-15", asOf: "2025-12-31", wantError: loan.ErrHistoricalEvidence, wantDWH: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract := loan.ContractData{
				PrimaryAccount:  "primary",
				FlatRatePercent: loan.MustMoney("12.50"),
			}
			if test.closeDate != "" {
				contract.CloseDate = parseDate(test.closeDate, location)
			}
			fincloud := &fincloudFake{contract: contract}
			mso := &msoFake{}
			position := loan.LoanPosition{
				AsOf: parseDate(test.asOf, location), LoanStartDate: parseDate(test.loanStartDate, location), AccountNumber: "primary",
				PrincipalOutstanding: loan.MustMoney("76543210"), PrincipalDue: loan.MustMoney("100000"),
				InterestDue: loan.MustMoney("20000"), PenaltyDue: loan.MustMoney("5000"), CollectabilityBI: 2, Source: test.wantSource,
			}
			dwh := &dwhFake{exact: position}
			snapshot := &snapshotFake{position: position}
			calculator := &calculatorFake{}
			service, _ := NewService(fincloud, mso, dwh, snapshot, calculator, location)
			service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }
			result, err := service.GetLoanPosition(context.Background(), "primary", parseDate(test.asOf, location))
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) {
					t.Fatalf("error=%v want=%v", err, test.wantError)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if result.Position.Source != test.wantSource || !reflect.DeepEqual(result.Loan, contract) {
					t.Fatalf("result=%+v", result)
				}
				if test.wantSource == loan.SourceClosed {
					if !result.Position.PrincipalOutstanding.IsZero() || !result.Position.PrincipalDue.IsZero() || !result.Position.InterestDue.IsZero() || !result.ActualPosition.PrincipalDue.IsZero() || !result.ActualPosition.InterestDue.IsZero() || !result.ActualPosition.PenaltyDue.IsZero() {
						t.Fatalf("closed position=%+v", result.Position)
					}
				} else if !reflect.DeepEqual(result.Position, position) || !reflect.DeepEqual(result.ActualPosition, position) {
					t.Fatalf("position=%+v actual=%+v want=%+v", result.Position, result.ActualPosition, position)
				}
			}
			if fincloud.calls != 1 || mso.historicalCalls != 0 || mso.openingCalls != 0 ||
				dwh.exactCalls != test.wantDWH || dwh.timelineCalls != 0 || snapshot.calls != test.wantSnapshot || calculator.calls != 0 {
				t.Fatalf("calls: Fincloud=%d MSO=%d/%d DWH=%d/%d snapshot=%d calculator=%d",
					fincloud.calls, mso.historicalCalls, mso.openingCalls, dwh.exactCalls, dwh.timelineCalls, snapshot.calls, calculator.calls)
			}
			if test.wantDWH == 1 && dwh.exactAccount != "primary" || test.wantSnapshot == 1 && snapshot.account != "primary" {
				t.Fatalf("lookup keys: DWH=%q snapshot=%q", dwh.exactAccount, snapshot.account)
			}
		})
	}
}

func TestPositionServiceRequiresPositiveLineageEvidence(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	asOf := parseDate("2026-08-31", location)
	for _, test := range []struct {
		name             string
		contract         loan.ContractData
		missingStart     bool
		msoError         error
		dwhError         error
		wantError        error
		wantMSO, wantDWH int
	}{
		{name: "missing original start", contract: loan.ContractData{PrimaryAccount: "primary", AlternateAccount: "alternate"}, missingStart: true, wantError: loan.ErrHistoricalEvidence, wantDWH: 1},
		{name: "migrated missing MSO opening", contract: loan.ContractData{PrimaryAccount: "primary", AlternateAccount: "alternate"}, msoError: loan.ErrHistoricalEvidence, wantError: loan.ErrHistoricalEvidence, wantMSO: 1, wantDWH: 1},
		{name: "native missing DWH exact row", contract: loan.ContractData{PrimaryAccount: "primary"}, dwhError: errors.Join(loan.ErrNotFound, loan.ErrHistoricalEvidence), wantError: loan.ErrHistoricalEvidence, wantDWH: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			mso := &msoFake{openingError: test.msoError}
			dwh := &dwhFake{exact: loan.LoanPosition{LoanStartDate: parseDate("2024-06-01", location)}, exactError: test.dwhError}
			if test.missingStart {
				dwh.exact.LoanStartDate = loan.Date{}
			}
			snapshot, calculator := &snapshotFake{}, &calculatorFake{}
			service, _ := NewService(&fincloudFake{contract: test.contract}, mso, dwh, snapshot, calculator, location)
			service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }
			_, err := service.GetLoanPosition(context.Background(), "primary", asOf)
			if !errors.Is(err, test.wantError) || mso.historicalCalls != 0 || mso.openingCalls != test.wantMSO ||
				dwh.exactCalls != test.wantDWH || dwh.timelineCalls != 0 || snapshot.calls != 0 || calculator.calls != 0 {
				t.Fatalf("error=%v calls: MSO=%d/%d DWH=%d/%d snapshot=%d calculator=%d",
					err, mso.historicalCalls, mso.openingCalls, dwh.exactCalls, dwh.timelineCalls, snapshot.calls, calculator.calls)
			}
		})
	}
}

func TestPositionServiceReconstructsFlatLoanWithPreCutoffRestructuringMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/admin/access/login":
			_, _ = writer.Write([]byte(`{"status":"ok","data":{"result":{"sessionid":"session"}}}`))
		case "/pinjaman/inquiry/rekening/pinjaman":
			_, _ = writer.Write([]byte(`{"status":"ok","data":{"result":{"id":"primary","noalt":"0130112345","tgl_pencairan":"2025-10-13","plafondlimit":"100","jangkawaktu":"1 bulan","bungaflat":"12","restruktur_tanggalakhirakad":{"date":"2026-02-15 00:00:00.000000","timezone_type":3,"timezone":"Asia/Jakarta"},"jadwalangsuran":[{"angsuranke":1,"tanggal":"2025-11-12"}]}}}`))
		case "/admin/access/logout":
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client, err := fincloud.NewClient(fincloud.Config{BaseURL: server.URL, Username: "system", Password: "secret", LocationID: "000", RoleID: "R-1", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())

	location := time.FixedZone("Jakarta", 7*60*60)
	mso := &msoFake{opening: loan.OpeningLoanState{InterestType: FlatInterestType, PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 1}}
	dwh := &dwhFake{exact: loan.LoanPosition{LoanStartDate: parseDate("2024-06-01", location)}}
	snapshot := &snapshotFake{}
	calculator := &calculatorFake{result: loan.CalculationResult{PrincipalOutstanding: loan.MustMoney("75"), CollectabilityBI: 1}}
	service, _ := NewService(client, mso, dwh, snapshot, calculator, location)
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }

	result, err := service.GetLoanPosition(context.Background(), "primary", parseDate("2026-09-13", location))
	if err != nil {
		t.Fatal(err)
	}
	if result.Position.Source != loan.SourceReconstructed || result.Position.LoanStartDate.String() != "2024-06-01" || mso.openingCalls != 1 || calculator.calls != 1 || dwh.exactCalls != 1 || dwh.timelineCalls != 1 || snapshot.calls != 0 {
		t.Fatalf("source=%s calculator=%d exact=%d timeline=%d snapshot=%d", result.Position.Source, calculator.calls, dwh.exactCalls, dwh.timelineCalls, snapshot.calls)
	}
}

func TestPositionServiceRejectsFutureBeforeUpstreamCall(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	fincloud := &fincloudFake{}
	service, _ := NewService(fincloud, &msoFake{}, &dwhFake{}, &snapshotFake{}, &calculatorFake{}, location)
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }
	_, err := service.GetLoanPosition(context.Background(), "account", parseDate("2026-09-15", location))
	if !errors.Is(err, loan.ErrInvalidInput) || fincloud.calls != 0 {
		t.Fatalf("error=%v calls=%d", err, fincloud.calls)
	}
}

func TestPositionServiceReconstructsNormalizedTenorPlusOneSchedule(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	evidence := make([]loan.ContractualInstallmentEvidence, 61)
	firstDue := time.Date(2021, time.October, 20, 0, 0, 0, 0, location)
	for index := range evidence {
		evidence[index] = loan.ContractualInstallmentEvidence{Number: int64(index), RawDueDate: firstDue.AddDate(0, index-1, 0).Format(loan.DateLayout)}
	}
	contract := loan.ContractData{
		PrimaryAccount: "primary", AlternateAccount: "alternate", PlafondLimit: loan.MustMoney("100000000"), TenorMonths: 60,
		FlatRatePercent: loan.MustMoney("7.8"), RawScheduleCount: 61, ContractScheduleEvidence: evidence,
	}
	mso := &msoFake{opening: loan.OpeningLoanState{InterestType: FlatInterestType, PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 1}}
	dwh := &dwhFake{}
	snapshot := &snapshotFake{position: loan.LoanPosition{LoanStartDate: parseDate("2024-06-01", location), Source: loan.SourceTodaySnapshot, CollectabilityBI: 2}}
	calculator := &calculatorFake{result: loan.CalculationResult{PrincipalOutstanding: loan.MustMoney("50"), CollectabilityBI: 1}}
	service, _ := NewService(&fincloudFake{contract: contract}, mso, dwh, snapshot, calculator, location)
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }

	result, err := service.GetLoanPosition(context.Background(), "primary", parseDate("2026-09-14", location))
	if err != nil {
		t.Fatal(err)
	}
	if result.Position.Source != loan.SourceReconstructed || calculator.calls != 1 || dwh.exactCalls != 0 || snapshot.calls != 1 {
		t.Fatalf("source=%s calculator=%d exact=%d snapshot=%d", result.Position.Source, calculator.calls, dwh.exactCalls, snapshot.calls)
	}
	if result.Loan.RawScheduleCount != 61 || len(result.Loan.ContractSchedule) != 60 || len(calculator.input.ContractSchedule) != 60 {
		t.Fatalf("raw=%d normalized=%d calculator=%d", result.Loan.RawScheduleCount, len(result.Loan.ContractSchedule), len(calculator.input.ContractSchedule))
	}
}

func TestPositionServiceRejectsInvalidFlatContractSchedule(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	contract := loan.ContractData{PrimaryAccount: "primary", AlternateAccount: "alternate", PlafondLimit: loan.MustMoney("100"), TenorMonths: 2, FlatRatePercent: loan.MustMoney("12"), ContractScheduleEvidence: []loan.ContractualInstallmentEvidence{{Number: 1, RawDueDate: "bad"}}}
	mso := &msoFake{opening: loan.OpeningLoanState{InterestType: FlatInterestType, PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 1}}
	dwh := &dwhFake{exact: loan.LoanPosition{LoanStartDate: parseDate("2024-06-01", location)}}
	snapshot := &snapshotFake{}
	calculator := &calculatorFake{}
	service, _ := NewService(&fincloudFake{contract: contract}, mso, dwh, snapshot, calculator, location)
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }

	_, err := service.GetLoanPosition(context.Background(), "primary", parseDate("2026-09-13", location))
	if !errors.Is(err, loan.ErrUnsupportedCalculation) || dwh.exactCalls != 1 || dwh.timelineCalls != 0 || snapshot.calls != 0 || calculator.calls != 0 {
		t.Fatalf("error=%v exact=%d timeline=%d snapshot=%d calculator=%d", err, dwh.exactCalls, dwh.timelineCalls, snapshot.calls, calculator.calls)
	}
}

func TestPositionServiceNeverFallsBackFromMissingEvidence(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	contract := loan.ContractData{PrimaryAccount: "primary", AlternateAccount: "alternate", PlafondLimit: loan.MustMoney("100"), TenorMonths: 1, FlatRatePercent: loan.MustMoney("12"), ContractSchedule: []loan.ContractualInstallment{{Number: 1, DueDate: parseDate("2025-11-12", location)}}}
	mso := &msoFake{opening: loan.OpeningLoanState{AccountNumber: "primary", InterestType: "10", PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 1}}
	dwh := &dwhFake{exact: loan.LoanPosition{LoanStartDate: parseDate("2024-06-01", location)}, timelineError: loan.ErrDWHUnavailable}
	snapshot := &snapshotFake{position: loan.LoanPosition{LoanStartDate: parseDate("2024-06-01", location), PrincipalOutstanding: loan.MustMoney("999"), CollectabilityBI: 1}}
	service, _ := NewService(&fincloudFake{contract: contract}, mso, dwh, snapshot, &calculatorFake{}, location)
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }
	_, err := service.GetLoanPosition(context.Background(), "account", parseDate("2026-09-13", location))
	if !errors.Is(err, loan.ErrDWHUnavailable) || snapshot.calls != 0 {
		t.Fatalf("error=%v snapshot calls=%d", err, snapshot.calls)
	}

	mso.opening.InterestType = "20"
	snapshot.err = errors.Join(loan.ErrNotFound, loan.ErrCurrentSnapshot)
	_, err = service.GetLoanPosition(context.Background(), "account", parseDate("2026-09-14", location))
	if !errors.Is(err, loan.ErrCurrentSnapshot) {
		t.Fatalf("error = %v", err)
	}
}

func TestPositionServiceRejectsMissingAlternateBeforeMSOLookup(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	mso := &msoFake{}
	service, _ := NewService(&fincloudFake{contract: loan.ContractData{PrimaryAccount: "3080020000000094"}}, mso, &dwhFake{}, &snapshotFake{}, &calculatorFake{}, location)
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }

	_, err := service.GetLoanPosition(context.Background(), "input", parseDate("2025-10-11", location))
	if !errors.Is(err, loan.ErrHistoricalEvidence) || mso.historicalCalls != 0 || mso.openingCalls != 0 {
		t.Fatalf("error=%v MSO historical=%d opening=%d", err, mso.historicalCalls, mso.openingCalls)
	}
}

func TestNormalizeContractScheduleIgnoresInstallmentZeroAndSorts(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	schedule, err := normalizeContractSchedule([]loan.ContractualInstallmentEvidence{
		{Number: 2, RawDueDate: "2026-03-01"},
		{Number: 0, RawDueDate: "invalid but excluded"},
		{Number: 3, RawDueDate: "2026-04-01"},
		{Number: 1, RawDueDate: "2026-02-01"},
	}, 3, location)
	if err != nil {
		t.Fatal(err)
	}
	for index, wantDate := range []string{"2026-02-01", "2026-03-01", "2026-04-01"} {
		if schedule[index].Number != index+1 || schedule[index].DueDate.String() != wantDate {
			t.Fatalf("schedule[%d] = %+v", index, schedule[index])
		}
	}
}

func TestNormalizeContractScheduleRejectsInvalidEvidence(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	valid := []loan.ContractualInstallmentEvidence{{Number: 0, RawDueDate: "2026-01-01"}, {Number: 1, RawDueDate: "2026-02-01"}, {Number: 2, RawDueDate: "2026-03-01"}, {Number: 3, RawDueDate: "2026-04-01"}}
	for _, test := range []struct {
		name string
		rows []loan.ContractualInstallmentEvidence
	}{
		{name: "duplicate installment", rows: append(append([]loan.ContractualInstallmentEvidence(nil), valid...), loan.ContractualInstallmentEvidence{Number: 1, RawDueDate: "2026-02-02"})},
		{name: "missing installment", rows: []loan.ContractualInstallmentEvidence{{Number: 0, RawDueDate: "2026-01-01"}, {Number: 1, RawDueDate: "2026-02-01"}, {Number: 3, RawDueDate: "2026-04-01"}}},
		{name: "installment over tenor", rows: append(append([]loan.ContractualInstallmentEvidence(nil), valid...), loan.ContractualInstallmentEvidence{Number: 4, RawDueDate: "2026-05-01"})},
		{name: "negative installment", rows: append(append([]loan.ContractualInstallmentEvidence(nil), valid...), loan.ContractualInstallmentEvidence{Number: -1, RawDueDate: "2025-12-01"})},
		{name: "duplicate date", rows: []loan.ContractualInstallmentEvidence{{Number: 0, RawDueDate: "2026-01-01"}, {Number: 1, RawDueDate: "2026-02-01"}, {Number: 2, RawDueDate: "2026-02-01"}, {Number: 3, RawDueDate: "2026-04-01"}}},
		{name: "non chronological date", rows: []loan.ContractualInstallmentEvidence{{Number: 0, RawDueDate: "2026-01-01"}, {Number: 1, RawDueDate: "2026-03-01"}, {Number: 2, RawDueDate: "2026-02-01"}, {Number: 3, RawDueDate: "2026-04-01"}}},
		{name: "invalid date", rows: []loan.ContractualInstallmentEvidence{{Number: 0, RawDueDate: "2026-01-01"}, {Number: 1, RawDueDate: "bad"}, {Number: 2, RawDueDate: "2026-03-01"}, {Number: 3, RawDueDate: "2026-04-01"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeContractSchedule(test.rows, 3, location)
			if !errors.Is(err, loan.ErrUnsupportedCalculation) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestFormatFincloudAltNoToMSO(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{input: "0130112345", want: "01.301.12345"},
		{input: "123", want: "123"},
	} {
		if got := formatFincloudAltNoToMSO(test.input); got != test.want {
			t.Errorf("formatFincloudAltNoToMSO(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func parseDate(value string, location *time.Location) loan.Date {
	date, err := loan.ParseDate(value, location)
	if err != nil {
		panic(err)
	}
	return date
}
