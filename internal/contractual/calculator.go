package contractual

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ibldzn/trs/internal/loan"
)

type Calculator struct {
	// Round is isolated pending validation against MSO. Nil keeps exact rational values.
	Round func(loan.Money) loan.Money
}

func (calculator Calculator) Calculate(input loan.CalculationInput) (loan.CalculationResult, error) {
	if input.AsOf.IsZero() || input.Cutoff.IsZero() || input.AsOf.Before(input.Cutoff) {
		return loan.CalculationResult{}, fmt.Errorf("%w: as-of date must be on or after cutoff", loan.ErrInvalidInput)
	}
	if err := validateOpening(input.Opening); err != nil {
		return loan.CalculationResult{}, err
	}

	state := calculationState{
		principal:    input.Opening.PrincipalOutstanding,
		principalDue: input.Opening.PrincipalDue,
		interestDue:  input.Opening.InterestDue,
	}
	result := loan.CalculationResult{
		AsOf:             input.AsOf,
		CollectabilityBI: input.Opening.CollectabilityBI,
		Trace:            loan.CalculationTrace{Opening: input.Opening},
	}
	if input.AsOf.Equal(input.Cutoff) {
		return finish(result, state)
	}
	if input.ContractualPrincipal.IsNegative() || input.ContractualPrincipal.IsZero() || input.TenorMonths <= 0 || input.FlatRatePercent.IsNegative() {
		return loan.CalculationResult{}, fmt.Errorf("%w: invalid contract parameters", loan.ErrInvariant)
	}

	dueDates := append([]loan.Date(nil), input.DueDates...)
	slices.SortFunc(dueDates, compareDate)
	if len(dueDates) != input.TenorMonths {
		return loan.CalculationResult{}, fmt.Errorf("%w: got %d due dates for %d-month tenor", loan.ErrUnsupportedCalculation, len(dueDates), input.TenorMonths)
	}
	for index, date := range dueDates {
		if date.IsZero() || (index > 0 && date.Equal(dueDates[index-1])) {
			return loan.CalculationResult{}, fmt.Errorf("%w: invalid or duplicate contractual due date", loan.ErrInvariant)
		}
	}

	principalPerPeriod, err := input.ContractualPrincipal.DivInt(int64(input.TenorMonths))
	if err != nil {
		return loan.CalculationResult{}, fmt.Errorf("%w: principal installment: %v", loan.ErrInvariant, err)
	}
	interestPerPeriod := input.ContractualPrincipal.Mul(input.FlatRatePercent)
	interestPerPeriod, err = interestPerPeriod.DivInt(100)
	if err == nil {
		interestPerPeriod, err = interestPerPeriod.DivInt(12)
	}
	if err != nil {
		return loan.CalculationResult{}, fmt.Errorf("%w: interest installment: %v", loan.ErrInvariant, err)
	}
	principalPerPeriod = calculator.round(principalPerPeriod)
	interestPerPeriod = calculator.round(interestPerPeriod)
	if principalPerPeriod.IsNegative() || interestPerPeriod.IsNegative() {
		return loan.CalculationResult{}, fmt.Errorf("%w: rounding policy returned negative installment", loan.ErrInvariant)
	}

	timeline := append([]loan.CollectabilityPoint(nil), input.CollectabilityTimeline...)
	slices.SortStableFunc(timeline, func(left, right loan.CollectabilityPoint) int { return compareDate(left.Date, right.Date) })
	if err := validateTimeline(timeline); err != nil {
		return loan.CalculationResult{}, err
	}
	repayments := append([]loan.Repayment(nil), input.Repayments...)
	slices.SortStableFunc(repayments, func(left, right loan.Repayment) int { return compareDate(left.Date, right.Date) })

	nextDue := 0
	for nextDue < len(dueDates) && !dueDates[nextDue].After(input.Cutoff) {
		nextDue++
	}
	accrueThrough := func(date loan.Date) {
		for nextDue < len(dueDates) && !dueDates[nextDue].After(date) {
			if state.principal.IsPositive() {
				available := state.principal.Sub(state.principalDue)
				if available.IsPositive() {
					state.principalDue = state.principalDue.Add(loan.MinMoney(principalPerPeriod, available))
				}
				state.interestDue = state.interestDue.Add(interestPerPeriod)
			}
			result.Trace.PeriodsAccrued++
			result.Trace.LastDueDateAccrued = dueDates[nextDue]
			nextDue++
		}
	}

	for _, payment := range repayments {
		if !payment.Date.After(input.Cutoff) || payment.Date.After(input.AsOf) {
			continue
		}
		if payment.Date.IsZero() || payment.PrincipalComponent.IsNegative() || payment.InterestComponent.IsNegative() || payment.PenaltyComponent.IsNegative() || payment.EarlyPenaltyComponent.IsNegative() || payment.DWPComponent.IsNegative() || payment.TotalPayment.IsNegative() {
			return loan.CalculationResult{}, fmt.Errorf("%w: negative or invalid repayment on %s", loan.ErrUnsupportedCalculation, payment.Date)
		}
		accrueThrough(payment.Date)
		collectability, err := collectabilityAt(input.Opening.CollectabilityBI, timeline, payment.Date)
		if err != nil {
			return loan.CalculationResult{}, err
		}
		amount := payment.PrincipalComponent.Add(payment.InterestComponent)
		if amount.IsZero() {
			continue
		}
		state.allocate(amount, collectability)
		result.Trace.RepaymentsApplied++
		result.Trace.LastPaymentDate = payment.Date
	}
	accrueThrough(input.AsOf)

	collectability, err := collectabilityAt(input.Opening.CollectabilityBI, timeline, input.AsOf)
	if err != nil {
		return loan.CalculationResult{}, err
	}
	result.CollectabilityBI = collectability
	return finish(result, state)
}

func (calculator Calculator) round(value loan.Money) loan.Money {
	if calculator.Round == nil {
		return value
	}
	return calculator.Round(value)
}

type calculationState struct {
	principal    loan.Money
	principalDue loan.Money
	interestDue  loan.Money
	unapplied    loan.Money
}

func (state *calculationState) allocate(payment loan.Money, collectability int) {
	if collectability <= 2 {
		payment = payBucket(payment, &state.interestDue)
		payment = state.payPrincipalDue(payment)
		payment = state.payPrincipalAdvance(payment)
	} else {
		payment = state.payPrincipalDue(payment)
		payment = state.payPrincipalAdvance(payment)
		payment = payBucket(payment, &state.interestDue)
	}
	state.unapplied = state.unapplied.Add(payment)
}

func (state *calculationState) payPrincipalDue(payment loan.Money) loan.Money {
	paid := loan.MinMoney(payment, state.principalDue)
	state.principalDue = state.principalDue.Sub(paid)
	state.principal = state.principal.Sub(paid)
	return payment.Sub(paid)
}

func (state *calculationState) payPrincipalAdvance(payment loan.Money) loan.Money {
	paid := loan.MinMoney(payment, state.principal)
	state.principal = state.principal.Sub(paid)
	return payment.Sub(paid)
}

func payBucket(payment loan.Money, bucket *loan.Money) loan.Money {
	paid := loan.MinMoney(payment, *bucket)
	*bucket = bucket.Sub(paid)
	return payment.Sub(paid)
}

func finish(result loan.CalculationResult, state calculationState) (loan.CalculationResult, error) {
	if state.principal.IsNegative() || state.principalDue.IsNegative() || state.interestDue.IsNegative() || state.unapplied.IsNegative() || state.principalDue.Cmp(state.principal) > 0 {
		return loan.CalculationResult{}, fmt.Errorf("%w: invalid ending state", loan.ErrInvariant)
	}
	result.PrincipalOutstanding = state.principal
	result.PrincipalDue = state.principalDue
	result.InterestDue = state.interestDue
	result.UnappliedAmount = state.unapplied
	return result, nil
}

func validateOpening(opening loan.OpeningLoanState) error {
	if opening.PrincipalOutstanding.IsNegative() || opening.PrincipalDue.IsNegative() || opening.InterestDue.IsNegative() || opening.PrincipalDue.Cmp(opening.PrincipalOutstanding) > 0 {
		return fmt.Errorf("%w: invalid opening balances", loan.ErrInvariant)
	}
	if opening.CollectabilityBI < 1 || opening.CollectabilityBI > 5 {
		return fmt.Errorf("%w: opening collectability must be 1..5", loan.ErrInvariant)
	}
	return nil
}

func validateTimeline(timeline []loan.CollectabilityPoint) error {
	for index, point := range timeline {
		if point.Date.IsZero() || point.Value < 1 || point.Value > 5 {
			return fmt.Errorf("%w: invalid collectability timeline point", loan.ErrHistoricalEvidence)
		}
		if index > 0 && point.Date.Equal(timeline[index-1].Date) && point.Value != timeline[index-1].Value {
			return fmt.Errorf("%w: conflicting collectability values on %s", loan.ErrHistoricalEvidence, point.Date)
		}
	}
	return nil
}

func collectabilityAt(opening int, timeline []loan.CollectabilityPoint, date loan.Date) (int, error) {
	if opening < 1 || opening > 5 {
		return 0, fmt.Errorf("%w: invalid opening collectability", loan.ErrHistoricalEvidence)
	}
	value := opening
	for _, point := range timeline {
		if point.Date.After(date) {
			break
		}
		value = point.Value
	}
	if value < 1 || value > 5 {
		return 0, errors.Join(loan.ErrHistoricalEvidence, fmt.Errorf("invalid collectability for %s", date))
	}
	return value, nil
}

func compareDate(left, right loan.Date) int {
	if left.Before(right) {
		return -1
	}
	if left.After(right) {
		return 1
	}
	return 0
}
