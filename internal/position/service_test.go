package position

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

type fincloudFake struct {
	contract loan.ContractData
	calls    int
}

func (fake *fincloudFake) ResolveLoan(context.Context, string, *time.Location) (loan.ContractData, error) {
	fake.calls++
	return fake.contract, nil
}

type msoFake struct {
	historical        loan.LoanPosition
	opening           loan.OpeningLoanState
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
	return fake.opening, nil
}

type dwhFake struct {
	exact           loan.LoanPosition
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
	return fake.exact, nil
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
		interestType                                                                                string
		changed                                                                                     bool
		unusableSchedule                                                                            bool
		wantSource                                                                                  loan.PositionSource
		wantMSOHistorical, wantMSOOpening, wantDWHExact, wantTimeline, wantSnapshot, wantCalculator int
	}{
		{name: "pre-cutoff uses MSO", asOf: "2025-10-11", interestType: "10", wantSource: loan.SourceMSO, wantMSOHistorical: 1},
		{name: "cutoff uses MSO opening", asOf: "2025-10-12", interestType: "10", wantSource: loan.SourceMSO, wantMSOOpening: 1},
		{name: "non-contractual historical ignores unusable flat schedule", asOf: "2026-09-13", interestType: "20", unusableSchedule: true, wantSource: loan.SourceDWH, wantMSOOpening: 1, wantDWHExact: 1},
		{name: "non-contractual today uses current snapshot", asOf: "2026-09-14", interestType: "20", wantSource: loan.SourceTodaySnapshot, wantMSOOpening: 1, wantSnapshot: 1},
		{name: "contract change ignores unusable reconstruction schedule", asOf: "2026-09-13", interestType: "10", changed: true, unusableSchedule: true, wantSource: loan.SourceDWH, wantMSOOpening: 1, wantDWHExact: 1},
		{name: "reconstructed post-cutoff loan", asOf: "2026-09-13", interestType: "10", wantSource: loan.SourceReconstructed, wantMSOOpening: 1, wantTimeline: 1, wantCalculator: 1},
		{name: "supported today requires snapshot collectability", asOf: "2026-09-14", interestType: "10", wantSource: loan.SourceReconstructed, wantMSOOpening: 1, wantTimeline: 1, wantSnapshot: 1, wantCalculator: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			asOf := parseDate(test.asOf, location)
			contract := loan.ContractData{PrimaryAccount: "3080020000000094", AlternateAccount: "0130112345", PlafondLimit: loan.MustMoney("100"), TenorMonths: 1, FlatRatePercent: loan.MustMoney("12"), ContractChanged: test.changed, ContractSchedule: []loan.ContractualInstallment{{Number: 1, DueDate: parseDate("2025-11-12", location)}}}
			if test.unusableSchedule {
				contract.ContractSchedule = nil
				contract.ContractScheduleEvidence = []loan.ContractualInstallmentEvidence{{Number: 1, RawDueDate: "bad"}}
			}
			fincloud := &fincloudFake{contract: contract}
			mso := &msoFake{historical: loan.LoanPosition{AccountNumber: "01.301.12345", Source: loan.SourceMSO}, opening: loan.OpeningLoanState{AccountNumber: "01.301.12345", InterestType: test.interestType, PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 2}}
			dwh := &dwhFake{exact: loan.LoanPosition{Source: loan.SourceDWH}, timeline: []loan.CollectabilityPoint{{Date: parseDate("2026-09-13", location), Value: 3}}}
			snapshot := &snapshotFake{position: loan.LoanPosition{Source: loan.SourceTodaySnapshot, CollectabilityBI: 4}}
			calculator := &calculatorFake{result: loan.CalculationResult{AsOf: asOf, PrincipalOutstanding: loan.MustMoney("55"), CollectabilityBI: 4}}
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
				if calculator.input.Opening.CollectabilityBI != 2 || len(calculator.input.CollectabilityTimeline) != test.wantTimeline+test.wantSnapshot || calculator.input.ContractualPrincipal.Format(2) != "100.00" {
					t.Fatalf("calculator input = %+v", calculator.input)
				}
			}
		})
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
	snapshot := &snapshotFake{position: loan.LoanPosition{Source: loan.SourceTodaySnapshot, CollectabilityBI: 2}}
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
	dwh := &dwhFake{}
	snapshot := &snapshotFake{}
	calculator := &calculatorFake{}
	service, _ := NewService(&fincloudFake{contract: contract}, mso, dwh, snapshot, calculator, location)
	service.now = func() time.Time { return parseDate("2026-09-14", location).Time(location) }

	_, err := service.GetLoanPosition(context.Background(), "primary", parseDate("2026-09-13", location))
	if !errors.Is(err, loan.ErrUnsupportedCalculation) || dwh.exactCalls != 0 || dwh.timelineCalls != 0 || snapshot.calls != 0 || calculator.calls != 0 {
		t.Fatalf("error=%v exact=%d timeline=%d snapshot=%d calculator=%d", err, dwh.exactCalls, dwh.timelineCalls, snapshot.calls, calculator.calls)
	}
}

func TestPositionServiceNeverFallsBackFromMissingEvidence(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	contract := loan.ContractData{PrimaryAccount: "primary", AlternateAccount: "alternate", PlafondLimit: loan.MustMoney("100"), TenorMonths: 1, FlatRatePercent: loan.MustMoney("12"), ContractSchedule: []loan.ContractualInstallment{{Number: 1, DueDate: parseDate("2025-11-12", location)}}}
	mso := &msoFake{opening: loan.OpeningLoanState{AccountNumber: "primary", InterestType: "10", PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 1}}
	dwh := &dwhFake{timelineError: loan.ErrDWHUnavailable}
	snapshot := &snapshotFake{position: loan.LoanPosition{PrincipalOutstanding: loan.MustMoney("999"), CollectabilityBI: 1}}
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
