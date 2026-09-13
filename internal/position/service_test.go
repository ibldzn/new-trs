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
	historical      loan.LoanPosition
	opening         loan.OpeningLoanState
	historicalCalls int
	openingCalls    int
}

func (fake *msoFake) HistoricalPosition(context.Context, string, loan.Date) (loan.LoanPosition, error) {
	fake.historicalCalls++
	return fake.historical, nil
}
func (fake *msoFake) OpeningState(context.Context, string, loan.Date) (loan.OpeningLoanState, error) {
	fake.openingCalls++
	return fake.opening, nil
}

type dwhFake struct {
	exact         loan.LoanPosition
	timeline      []loan.CollectabilityPoint
	timelineError error
	exactCalls    int
	timelineCalls int
}

func (fake *dwhFake) ExactPosition(context.Context, string, loan.Date) (loan.LoanPosition, error) {
	fake.exactCalls++
	return fake.exact, nil
}
func (fake *dwhFake) CollectabilityTimeline(context.Context, string, loan.Date, loan.Date) ([]loan.CollectabilityPoint, error) {
	fake.timelineCalls++
	return fake.timeline, fake.timelineError
}

type snapshotFake struct {
	position loan.LoanPosition
	err      error
	calls    int
}

func (fake *snapshotFake) ExactPosition(context.Context, string, loan.Date) (loan.LoanPosition, error) {
	fake.calls++
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
		wantSource                                                                                  loan.PositionSource
		wantMSOHistorical, wantMSOOpening, wantDWHExact, wantTimeline, wantSnapshot, wantCalculator int
	}{
		{name: "pre-cutoff uses MSO", asOf: "2025-10-11", interestType: "10", wantSource: loan.SourceMSO, wantMSOHistorical: 1},
		{name: "cutoff uses MSO opening", asOf: "2025-10-12", interestType: "10", wantSource: loan.SourceMSO, wantMSOOpening: 1},
		{name: "non-contractual historical uses exact DWH", asOf: "2026-09-13", interestType: "20", wantSource: loan.SourceDWH, wantMSOOpening: 1, wantDWHExact: 1},
		{name: "non-contractual today uses current snapshot", asOf: "2026-09-14", interestType: "20", wantSource: loan.SourceTodaySnapshot, wantMSOOpening: 1, wantSnapshot: 1},
		{name: "contract change uses exact DWH", asOf: "2026-09-13", interestType: "10", changed: true, wantSource: loan.SourceDWH, wantMSOOpening: 1, wantDWHExact: 1},
		{name: "supported historical reconstructs", asOf: "2026-09-13", interestType: "10", wantSource: loan.SourceReconstructed, wantMSOOpening: 1, wantTimeline: 1, wantCalculator: 1},
		{name: "supported today requires snapshot collectability", asOf: "2026-09-14", interestType: "10", wantSource: loan.SourceReconstructed, wantMSOOpening: 1, wantTimeline: 1, wantSnapshot: 1, wantCalculator: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			asOf := parseDate(test.asOf, location)
			contract := loan.ContractData{PrimaryAccount: "primary", PlafondLimit: loan.MustMoney("100"), TenorMonths: 1, FlatRatePercent: loan.MustMoney("12"), ContractChanged: test.changed, DueDates: []loan.Date{parseDate("2025-11-12", location)}}
			fincloud := &fincloudFake{contract: contract}
			mso := &msoFake{historical: loan.LoanPosition{Source: loan.SourceMSO}, opening: loan.OpeningLoanState{AccountNumber: "primary", InterestType: test.interestType, PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 2}}
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

func TestPositionServiceNeverFallsBackFromMissingEvidence(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	contract := loan.ContractData{PrimaryAccount: "primary", PlafondLimit: loan.MustMoney("100"), TenorMonths: 1, FlatRatePercent: loan.MustMoney("12"), DueDates: []loan.Date{parseDate("2025-11-12", location)}}
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

func parseDate(value string, location *time.Location) loan.Date {
	date, err := loan.ParseDate(value, location)
	if err != nil {
		panic(err)
	}
	return date
}
