package contractual

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ibldzn/trs/internal/loan"
)

func TestExactAdjacentReversalCancelsBeforeAllocation(t *testing.T) {
	for _, collectability := range []int{1, 2, 3, 4, 5} {
		input := baseInput()
		input.AsOf = date("2025-11-12")
		input.Opening.CollectabilityBI = collectability
		withoutPair, err := (Calculator{}).Calculate(input)
		if err != nil {
			t.Fatal(err)
		}
		positive := payment("2025-11-12", "300000", "0", "300000")
		positive.JournalNumber, positive.SourceOrder = "original", 0
		negative := payment("2025-11-12", "-300000", "0", "-300000")
		negative.JournalNumber, negative.SourceOrder = "different journal", 1
		input.Repayments = []loan.Repayment{positive, negative}
		original := append([]loan.Repayment(nil), input.Repayments...)
		withPair, err := (Calculator{}).Calculate(input)
		if err != nil || !reflect.DeepEqual(withPair, withoutPair) || !reflect.DeepEqual(input.Repayments, original) ||
			withPair.Trace.RepaymentsApplied != 0 || withPair.Trace.PeriodsAccrued != 1 ||
			!withPair.Trace.LastDueDateAccrued.Equal(input.AsOf) {
			t.Fatalf("collectability=%d result=%+v baseline=%+v repayments=%+v error=%v", collectability, withPair, withoutPair, input.Repayments, err)
		}
	}
}

func TestMixedSignPositiveRepaymentUsesNetAllocation(t *testing.T) {
	for _, collectability := range []int{1, 2, 3, 4, 5} {
		asOf := date("2026-08-26")
		row := payment("2026-08-26", "336501", "-4001", "332500")
		input := loan.CalculationInput{
			AsOf: asOf, Cutoff: date("2025-10-12"), ContractualPrincipal: money("1000000"),
			TenorMonths: 1, FlatRatePercent: money("18"),
			Opening: loan.OpeningLoanState{PrincipalOutstanding: money("1000000"), PrincipalDue: money("400000"),
				InterestDue: money("400000"), CollectabilityBI: 1},
			CollectabilityTimeline: []loan.CollectabilityPoint{{Date: date("2026-08-20"), Value: collectability}},
			ContractSchedule:       []loan.ContractualInstallment{{Number: 1, DueDate: date("2026-09-01")}},
			Repayments:             []loan.Repayment{row},
		}
		got, err := (Calculator{}).Calculate(input)
		if err != nil {
			t.Fatalf("collectability=%d: %v", collectability, err)
		}
		want := [3]string{"1000000.00", "400000.00", "67500.00"}
		if collectability >= 3 {
			want = [3]string{"667500.00", "67500.00", "400000.00"}
		}
		balances := [3]string{got.PrincipalOutstanding.Format(2), got.PrincipalDue.Format(2), got.InterestDue.Format(2)}
		if balances != want || got.Trace.RepaymentsApplied != 1 || !got.Trace.LastPaymentDate.Equal(asOf) || got.CollectabilityBI != collectability {
			t.Fatalf("collectability=%d balances=%v trace=%+v", collectability, balances, got.Trace)
		}
	}
}

func TestMixedSignPositiveExactInverseCancels(t *testing.T) {
	input := baseInput()
	input.AsOf = date("2025-11-12")
	withoutPair, err := (Calculator{}).Calculate(input)
	if err != nil {
		t.Fatal(err)
	}
	positive := payment("2025-11-12", "336501", "-4001", "332500")
	positive.JournalNumber, positive.SourceOrder = "original", 0
	negative := payment("2025-11-12", "-336501", "4001", "-332500")
	negative.JournalNumber, negative.SourceOrder = "different journal", 1
	input.Repayments = []loan.Repayment{positive, negative}
	original := append([]loan.Repayment(nil), input.Repayments...)
	withPair, err := (Calculator{}).Calculate(input)
	if err != nil || !reflect.DeepEqual(withPair, withoutPair) || !reflect.DeepEqual(input.Repayments, original) || withPair.Trace.RepaymentsApplied != 0 {
		t.Fatalf("result=%+v baseline=%+v repayments=%+v error=%v", withPair, withoutPair, input.Repayments, err)
	}
}

func TestZeroNetAdjustmentAndNegativeAllocable(t *testing.T) {
	for _, test := range []struct {
		name string
		row  loan.Repayment
		want error
	}{
		{"zero-net components", payment("2025-11-12", "100", "-100", "0"), loan.ErrUnsupportedRepaymentAdjustment},
		{"negative allocable despite positive total", func() loan.Repayment {
			row := payment("2025-11-12", "-200", "50", "100")
			row.PenaltyComponent = money("250")
			return row
		}(), loan.ErrUnsupportedCalculation},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := baseInput()
			input.AsOf = date("2025-11-12")
			input.Repayments = []loan.Repayment{test.row}
			_, err := (Calculator{}).Calculate(input)
			if !errors.Is(err, test.want) || errors.Is(err, loan.ErrUnsupportedRepaymentReversal) ||
				(test.want == loan.ErrUnsupportedCalculation && errors.Is(err, loan.ErrUnsupportedRepaymentAdjustment)) ||
				(test.want == loan.ErrUnsupportedRepaymentAdjustment && !errors.Is(err, loan.ErrUnsupportedCalculation)) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestObservedPatternLeavesOnlyPositiveInterestRows(t *testing.T) {
	input := baseInput()
	input.AsOf = date("2025-11-12")
	rows := []loan.Repayment{
		payment("2025-11-12", "300000", "0", "300000"),
		payment("2025-11-12", "-300000", "0", "-300000"),
		payment("2025-11-12", "0", "287693", "287693"),
		payment("2025-11-12", "0", "12307", "12307"),
	}
	for index := range rows {
		rows[index].SourceOrder = index
	}
	input.Repayments = rows[2:]
	want, err := (Calculator{}).Calculate(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Repayments = rows
	got, err := (Calculator{}).Calculate(input)
	if err != nil || !reflect.DeepEqual(got, want) || got.Trace.RepaymentsApplied != 2 {
		t.Fatalf("result=%+v baseline=%+v error=%v", got, want, err)
	}
}

func TestUnsupportedRepaymentReversals(t *testing.T) {
	positive := payment("2025-11-12", "300000", "0", "300000")
	negative := payment("2025-11-12", "-300000", "0", "-300000")
	for _, test := range []struct {
		name string
		rows []loan.Repayment
	}{
		{"negative without predecessor", []loan.Repayment{negative}},
		{"different date", []loan.Repayment{positive, payment("2025-12-12", "-300000", "0", "-300000")}},
		{"partial inverse", []loan.Repayment{positive, payment("2025-11-12", "-100000", "0", "-100000")}},
		{"same total different components", []loan.Repayment{positive, payment("2025-11-12", "0", "-300000", "-300000")}},
		{"non-adjacent inverse", []loan.Repayment{positive, payment("2025-11-12", "100000", "0", "100000"), negative}},
		{"negative total mismatch", []loan.Repayment{positive, payment("2025-11-12", "-300000", "0", "-200000")}},
		{"mixed signs", []loan.Repayment{positive, payment("2025-11-12", "-300000", "1", "-299999")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := baseInput()
			input.AsOf = date("2025-12-12")
			input.Repayments = test.rows
			_, err := (Calculator{}).Calculate(input)
			if !errors.Is(err, loan.ErrUnsupportedRepaymentReversal) || !errors.Is(err, loan.ErrUnsupportedCalculation) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestReversalComparesEveryMonetaryComponentExactly(t *testing.T) {
	positive := loan.Repayment{Date: date("2025-11-12"), PrincipalComponent: money("5"), InterestComponent: money("4"),
		PenaltyComponent: money("3"), EarlyPenaltyComponent: money("2"), DWPComponent: money("1"), TotalPayment: money("15")}
	negative := loan.Repayment{Date: positive.Date, PrincipalComponent: money("-5.00"), InterestComponent: money("-4.0"),
		PenaltyComponent: money("-3"), EarlyPenaltyComponent: money("-2"), DWPComponent: money("-1"), TotalPayment: money("-15")}
	input := baseInput()
	input.AsOf = positive.Date
	input.Repayments = []loan.Repayment{positive, negative}
	got, err := (Calculator{}).Calculate(input)
	if err != nil || got.Trace.RepaymentsApplied != 0 {
		t.Fatalf("exact components: result=%+v error=%v", got, err)
	}
	for _, test := range []struct {
		name   string
		change func(*loan.Repayment)
	}{
		{"principal", func(row *loan.Repayment) { row.PrincipalComponent = money("-4") }},
		{"interest", func(row *loan.Repayment) { row.InterestComponent = money("-3") }},
		{"penalty", func(row *loan.Repayment) { row.PenaltyComponent = money("-2") }},
		{"early penalty", func(row *loan.Repayment) { row.EarlyPenaltyComponent = money("-1") }},
		{"DWP", func(row *loan.Repayment) { row.DWPComponent = money("0") }},
		{"total", func(row *loan.Repayment) { row.TotalPayment = money("-14") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := negative
			test.change(&changed)
			input.Repayments = []loan.Repayment{positive, changed}
			_, err := (Calculator{}).Calculate(input)
			if !errors.Is(err, loan.ErrUnsupportedRepaymentReversal) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	positive.TotalPayment = money("14")
	negative.TotalPayment = money("-14")
	input.Repayments = []loan.Repayment{positive, negative}
	_, err = (Calculator{}).Calculate(input)
	if !errors.Is(err, loan.ErrHistoricalEvidence) {
		t.Fatalf("exact inverse with inconsistent components: error=%v", err)
	}
}

func TestReversalValidationUsesWindowAndSourceOrder(t *testing.T) {
	input := baseInput()
	input.AsOf = date("2026-08-31")
	effective := payment("2025-11-01", "50", "0", "50")
	input.Repayments = []loan.Repayment{effective}
	want, err := (Calculator{}).Calculate(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Repayments = []loan.Repayment{
		payment("2026-09-01", "-300000", "0", "-300000"),
		payment("2026-09-01", "100", "-100", "0"),
		payment("2025-10-12", "-300000", "0", "-300000"),
		payment("2025-10-12", "100", "-100", "0"),
		effective,
	}
	got, err := (Calculator{}).Calculate(input)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("window result=%+v baseline=%+v error=%v", got, want, err)
	}

	input = baseInput()
	input.AsOf = date("2025-11-12")
	positive := payment("2025-11-12", "300000", "0", "300000")
	positive.SourceOrder = 1
	negative := payment("2025-11-12", "-300000", "0", "-300000")
	negative.SourceOrder = 2
	interest := payment("2025-11-12", "0", "12", "12")
	interest.SourceOrder = 3
	input.Repayments = []loan.Repayment{interest}
	want, err = (Calculator{}).Calculate(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Repayments = []loan.Repayment{negative, interest, positive}
	original := append([]loan.Repayment(nil), input.Repayments...)
	got, err = (Calculator{}).Calculate(input)
	if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(input.Repayments, original) || got.Trace.RepaymentsApplied != 1 {
		t.Fatalf("source order result=%+v baseline=%+v repayments=%+v error=%v", got, want, input.Repayments, err)
	}
}
