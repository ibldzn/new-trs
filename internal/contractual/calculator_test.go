package contractual

import (
	"errors"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

var testLocation = time.FixedZone("Jakarta", 7*60*60)

func TestCalculatorContractualBehavior(t *testing.T) {
	tests := []struct {
		name         string
		change       func(*loan.CalculationInput)
		want         [4]string
		wantKolek    int
		wantPeriods  int
		wantPayments int
		wantError    error
	}{
		{name: "clean opening at cutoff", change: func(input *loan.CalculationInput) { input.AsOf = date("2025-10-12") }, want: [4]string{"200.00", "0.00", "0.00", "0.00"}, wantKolek: 1},
		{name: "opening principal arrears carried", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-10-12")
			input.Opening.PrincipalDue = money("25")
		}, want: [4]string{"200.00", "25.00", "0.00", "0.00"}, wantKolek: 1},
		{name: "opening interest arrears carried", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-10-12")
			input.Opening.InterestDue = money("7")
		}, want: [4]string{"200.00", "0.00", "7.00", "0.00"}, wantKolek: 1},
		{name: "payment before next due", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-01")
			input.Opening.PrincipalDue = money("20")
			input.Opening.InterestDue = money("10")
			input.Repayments = []loan.Repayment{payment("2025-11-01", "50", "0", "50")}
		}, want: [4]string{"160.00", "0.00", "0.00", "0.00"}, wantKolek: 1, wantPayments: 1},
		{name: "payment exactly on due date accrues first", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			input.Repayments = []loan.Repayment{payment("2025-11-12", "100", "12", "112")}
		}, want: [4]string{"100.00", "0.00", "0.00", "0.00"}, wantKolek: 1, wantPeriods: 1, wantPayments: 1},
		{name: "payment after multiple due dates", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-12-20")
			input.Repayments = []loan.Repayment{payment("2025-12-20", "200", "24", "224")}
		}, want: [4]string{"0.00", "0.00", "0.00", "0.00"}, wantKolek: 1, wantPeriods: 2, wantPayments: 1},
		{name: "multiple repayments same date", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			input.Repayments = []loan.Repayment{payment("2025-11-12", "50", "6", "56"), payment("2025-11-12", "50", "6", "56")}
		}, want: [4]string{"100.00", "0.00", "0.00", "0.00"}, wantKolek: 1, wantPeriods: 1, wantPayments: 2},
		{name: "kolek 1 interest first", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			input.Opening.InterestDue = money("50")
			input.Repayments = []loan.Repayment{payment("2025-11-12", "150", "0", "150")}
		}, want: [4]string{"112.00", "12.00", "0.00", "0.00"}, wantKolek: 1, wantPeriods: 1, wantPayments: 1},
		{name: "kolek 2 interest first", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			input.Opening.CollectabilityBI = 2
			input.Opening.InterestDue = money("50")
			input.Repayments = []loan.Repayment{payment("2025-11-12", "150", "0", "150")}
		}, want: [4]string{"112.00", "12.00", "0.00", "0.00"}, wantKolek: 2, wantPeriods: 1, wantPayments: 1},
		{name: "kolek 3 principal all way", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			input.Opening.CollectabilityBI = 3
			input.Opening.InterestDue = money("50")
			input.Repayments = []loan.Repayment{payment("2025-11-12", "150", "0", "150")}
		}, want: [4]string{"50.00", "0.00", "62.00", "0.00"}, wantKolek: 3, wantPeriods: 1, wantPayments: 1},
		{name: "kolek 4 principal first", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			input.Opening.CollectabilityBI = 4
			input.Opening.InterestDue = money("50")
			input.Repayments = []loan.Repayment{payment("2025-11-12", "150", "0", "150")}
		}, want: [4]string{"50.00", "0.00", "62.00", "0.00"}, wantKolek: 4, wantPeriods: 1, wantPayments: 1},
		{name: "kolek 5 principal first", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			input.Opening.CollectabilityBI = 5
			input.Opening.InterestDue = money("50")
			input.Repayments = []loan.Repayment{payment("2025-11-12", "150", "0", "150")}
		}, want: [4]string{"50.00", "0.00", "62.00", "0.00"}, wantKolek: 5, wantPeriods: 1, wantPayments: 1},
		{name: "kolek transition 2 to 3", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-12-01")
			input.Opening.CollectabilityBI = 2
			input.Opening.PrincipalDue = money("100")
			input.Opening.InterestDue = money("100")
			input.CollectabilityTimeline = []loan.CollectabilityPoint{{Date: date("2025-11-15"), Value: 3}}
			input.Repayments = []loan.Repayment{payment("2025-11-01", "50", "0", "50"), payment("2025-12-01", "50", "0", "50")}
		}, want: [4]string{"150.00", "150.00", "62.00", "0.00"}, wantKolek: 3, wantPeriods: 1, wantPayments: 2},
		{name: "kolek transition 3 to 2", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-12-01")
			input.Opening.CollectabilityBI = 3
			input.Opening.PrincipalDue = money("100")
			input.Opening.InterestDue = money("100")
			input.CollectabilityTimeline = []loan.CollectabilityPoint{{Date: date("2025-11-15"), Value: 2}}
			input.Repayments = []loan.Repayment{payment("2025-11-01", "50", "0", "50"), payment("2025-12-01", "50", "0", "50")}
		}, want: [4]string{"150.00", "150.00", "62.00", "0.00"}, wantKolek: 2, wantPeriods: 1, wantPayments: 2},
		{name: "excess posting reduces principal", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-01")
			input.Repayments = []loan.Repayment{payment("2025-11-01", "50", "0", "50")}
		}, want: [4]string{"150.00", "0.00", "0.00", "0.00"}, wantKolek: 1, wantPayments: 1},
		{name: "payment over all balances is explicit", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			input.Repayments = []loan.Repayment{payment("2025-11-12", "500", "0", "500")}
		}, want: [4]string{"0.00", "0.00", "0.00", "288.00"}, wantKolek: 1, wantPeriods: 1, wantPayments: 1},
		{name: "penalty and DWP excluded", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-12")
			row := payment("2025-11-12", "100", "12", "999")
			row.PenaltyComponent = money("500")
			row.DWPComponent = money("300")
			input.Repayments = []loan.Repayment{row}
		}, want: [4]string{"100.00", "0.00", "0.00", "0.00"}, wantKolek: 1, wantPeriods: 1, wantPayments: 1},
		{name: "negative principal reversal rejected", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-01")
			input.Repayments = []loan.Repayment{payment("2025-11-01", "-1", "0", "-1")}
		}, wantError: loan.ErrUnsupportedCalculation},
		{name: "negative total reversal rejected", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-01")
			input.Repayments = []loan.Repayment{payment("2025-11-01", "0", "0", "-1")}
		}, wantError: loan.ErrUnsupportedCalculation},
		{name: "negative excluded component still signals reversal", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-01")
			row := payment("2025-11-01", "0", "0", "0")
			row.DWPComponent = money("-1")
			input.Repayments = []loan.Repayment{row}
		}, wantError: loan.ErrUnsupportedCalculation},
		{name: "zero allocable payment skipped", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-01")
			input.Repayments = []loan.Repayment{payment("2025-11-01", "0", "0", "200")}
		}, want: [4]string{"200.00", "0.00", "0.00", "0.00"}, wantKolek: 1},
		{name: "no payment accrues through as-of", change: func(input *loan.CalculationInput) { input.AsOf = date("2025-12-31") }, want: [4]string{"200.00", "200.00", "24.00", "0.00"}, wantKolek: 1, wantPeriods: 2},
		{name: "final contractual installment", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-12-12")
			input.Repayments = []loan.Repayment{payment("2025-12-12", "200", "24", "224")}
		}, want: [4]string{"0.00", "0.00", "0.00", "0.00"}, wantKolek: 1, wantPeriods: 2, wantPayments: 1},
		{name: "no synthetic installment after maturity", change: func(input *loan.CalculationInput) { input.AsOf = date("2027-12-31") }, want: [4]string{"200.00", "200.00", "24.00", "0.00"}, wantKolek: 1, wantPeriods: 2},
		{name: "prepayment caps future principal due", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-12-31")
			input.Repayments = []loan.Repayment{payment("2025-11-01", "150", "0", "150")}
		}, want: [4]string{"50.00", "50.00", "24.00", "0.00"}, wantKolek: 1, wantPeriods: 2, wantPayments: 1},
		{name: "MSO collectability remains fallback", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-01")
			input.Opening.CollectabilityBI = 4
			input.Repayments = []loan.Repayment{payment("2025-11-01", "50", "0", "50")}
		}, want: [4]string{"150.00", "0.00", "0.00", "0.00"}, wantKolek: 4, wantPayments: 1},
		{name: "future timeline does not replace fallback early", change: func(input *loan.CalculationInput) {
			input.AsOf = date("2025-11-20")
			input.Opening.CollectabilityBI = 2
			input.Opening.InterestDue = money("50")
			input.CollectabilityTimeline = []loan.CollectabilityPoint{{Date: date("2025-11-15"), Value: 3}}
			input.Repayments = []loan.Repayment{payment("2025-11-01", "50", "0", "50")}
		}, want: [4]string{"200.00", "100.00", "12.00", "0.00"}, wantKolek: 3, wantPeriods: 1, wantPayments: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := baseInput()
			test.change(&input)
			result, err := (Calculator{}).Calculate(input)
			if test.wantError != nil {
				if !errors.Is(err, test.wantError) {
					t.Fatalf("error = %v, want %v", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := [4]string{result.PrincipalOutstanding.Format(2), result.PrincipalDue.Format(2), result.InterestDue.Format(2), result.UnappliedAmount.Format(2)}
			if got != test.want || result.CollectabilityBI != test.wantKolek || result.Trace.PeriodsAccrued != test.wantPeriods || result.Trace.RepaymentsApplied != test.wantPayments {
				t.Fatalf("balances=%v kolek=%d periods=%d payments=%d", got, result.CollectabilityBI, result.Trace.PeriodsAccrued, result.Trace.RepaymentsApplied)
			}
		})
	}
}

func TestCalculatorRejectsInvalidEvidence(t *testing.T) {
	tests := []struct {
		name   string
		change func(*loan.CalculationInput)
		want   error
	}{
		{"before cutoff", func(input *loan.CalculationInput) { input.AsOf = date("2025-10-11") }, loan.ErrInvalidInput},
		{"invalid opening collectability", func(input *loan.CalculationInput) { input.Opening.CollectabilityBI = 0 }, loan.ErrInvariant},
		{"negative opening", func(input *loan.CalculationInput) { input.Opening.InterestDue = money("-1") }, loan.ErrInvariant},
		{"principal due over outstanding", func(input *loan.CalculationInput) { input.Opening.PrincipalDue = money("201") }, loan.ErrInvariant},
		{"incomplete schedule", func(input *loan.CalculationInput) { input.DueDates = input.DueDates[:11] }, loan.ErrUnsupportedCalculation},
		{"duplicate schedule", func(input *loan.CalculationInput) { input.DueDates[1] = input.DueDates[0] }, loan.ErrInvariant},
		{"invalid timeline", func(input *loan.CalculationInput) {
			input.CollectabilityTimeline = []loan.CollectabilityPoint{{Date: date("2025-11-01"), Value: 6}}
		}, loan.ErrHistoricalEvidence},
		{"conflicting timeline", func(input *loan.CalculationInput) {
			input.CollectabilityTimeline = []loan.CollectabilityPoint{{Date: date("2025-11-01"), Value: 2}, {Date: date("2025-11-01"), Value: 3}}
		}, loan.ErrHistoricalEvidence},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := baseInput()
			test.change(&input)
			_, err := (Calculator{}).Calculate(input)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func baseInput() loan.CalculationInput {
	dueDates := make([]loan.Date, 0, 12)
	for month := 1; month <= 12; month++ {
		dueDates = append(dueDates, loan.NewDate(time.Date(2025, time.Month(month), 12, 0, 0, 0, 0, testLocation), testLocation))
	}
	return loan.CalculationInput{
		AsOf: date("2025-10-13"), Cutoff: date("2025-10-12"), ContractualPrincipal: money("1200"), TenorMonths: 12,
		FlatRatePercent: money("12"), Opening: loan.OpeningLoanState{PrincipalOutstanding: money("200"), CollectabilityBI: 1}, DueDates: dueDates,
	}
}

func payment(day, principal, interest, total string) loan.Repayment {
	return loan.Repayment{Date: date(day), PrincipalComponent: money(principal), InterestComponent: money(interest), TotalPayment: money(total)}
}

func money(value string) loan.Money { return loan.MustMoney(value) }

func date(value string) loan.Date {
	parsed, err := loan.ParseDate(value, testLocation)
	if err != nil {
		panic(err)
	}
	return parsed
}
