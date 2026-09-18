package contractual

import (
	"cmp"
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
	schedule, err := calculator.BuildSchedule(input.ContractualPrincipal, input.TenorMonths, input.FlatRatePercent, input.ContractSchedule)
	if err != nil {
		return loan.CalculationResult{}, err
	}
	result.ContractualSchedule = schedule

	timeline := append([]loan.CollectabilityPoint(nil), input.CollectabilityTimeline...)
	slices.SortStableFunc(timeline, func(left, right loan.CollectabilityPoint) int { return compareDate(left.Date, right.Date) })
	if err := validateTimeline(timeline); err != nil {
		return loan.CalculationResult{}, err
	}
	repayments := append([]loan.Repayment(nil), input.Repayments...)
	slices.SortStableFunc(repayments, func(left, right loan.Repayment) int { return compareDate(left.Date, right.Date) })

	nextDue := 0
	for nextDue < len(schedule) && !schedule[nextDue].DueDate.After(input.Cutoff) {
		nextDue++
	}
	accrueThrough := func(date loan.Date) {
		for nextDue < len(schedule) && !schedule[nextDue].DueDate.After(date) {
			if state.principal.IsPositive() {
				available := state.principal.Sub(state.principalDue)
				if available.IsPositive() {
					state.principalDue = state.principalDue.Add(loan.MinMoney(schedule[nextDue].Principal, available))
				}
				state.interestDue = state.interestDue.Add(schedule[nextDue].Interest)
			}
			result.Trace.PeriodsAccrued++
			result.Trace.LastDueDateAccrued = schedule[nextDue].DueDate
			nextDue++
		}
	}

	for _, payment := range repayments {
		if !payment.Date.After(input.Cutoff) || payment.Date.After(input.AsOf) {
			continue
		}
		if payment.Date.IsZero() {
			return loan.CalculationResult{}, fmt.Errorf("%w: negative or invalid repayment on %s", loan.ErrUnsupportedCalculation, payment.Date)
		}
		amount := payment.PrincipalComponent.Add(payment.InterestComponent)
		if amount.Cmp(payment.TotalPayment) > 0 {
			return loan.CalculationResult{}, fmt.Errorf("%w: repayment principal and interest exceed total payment on %s", loan.ErrHistoricalEvidence, payment.Date)
		}
		accrueThrough(payment.Date)
		collectability, err := collectabilityAt(input.Opening.CollectabilityBI, timeline, payment.Date)
		if err != nil {
			return loan.CalculationResult{}, err
		}
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

func (calculator Calculator) BuildSchedule(principal loan.Money, tenor int, flatRate loan.Money, source []loan.ContractualInstallment) ([]loan.ContractualScheduleRow, error) {
	if principal.IsNegative() || principal.IsZero() || tenor <= 0 || flatRate.IsNegative() {
		return nil, fmt.Errorf("%w: invalid contract parameters", loan.ErrInvariant)
	}
	installments := append([]loan.ContractualInstallment(nil), source...)
	slices.SortFunc(installments, func(left, right loan.ContractualInstallment) int { return cmp.Compare(left.Number, right.Number) })
	if len(installments) != tenor {
		return nil, fmt.Errorf("%w: got %d contractual installments for %d-month tenor", loan.ErrUnsupportedCalculation, len(installments), tenor)
	}
	for index, installment := range installments {
		if installment.Number != index+1 || installment.DueDate.IsZero() {
			return nil, fmt.Errorf("%w: invalid contractual installment numbering", loan.ErrUnsupportedCalculation)
		}
		if index > 0 && !installment.DueDate.After(installments[index-1].DueDate) {
			return nil, fmt.Errorf("%w: contractual installment dates are not strictly chronological", loan.ErrUnsupportedCalculation)
		}
	}

	principalPerPeriod, err := principal.DivInt(int64(tenor))
	if err != nil {
		return nil, fmt.Errorf("%w: principal installment: %v", loan.ErrInvariant, err)
	}
	interestPerPeriod := principal.Mul(flatRate)
	interestPerPeriod, err = interestPerPeriod.DivInt(1200)
	if err != nil {
		return nil, fmt.Errorf("%w: interest installment: %v", loan.ErrInvariant, err)
	}
	principalPerPeriod = calculator.round(principalPerPeriod)
	interestPerPeriod = calculator.round(interestPerPeriod)
	if principalPerPeriod.IsNegative() || interestPerPeriod.IsNegative() {
		return nil, fmt.Errorf("%w: rounding policy returned negative installment", loan.ErrInvariant)
	}

	rows := make([]loan.ContractualScheduleRow, tenor)
	balance := principal
	for index, installment := range installments {
		periodPrincipal := principalPerPeriod
		if index == tenor-1 {
			periodPrincipal = balance
		} else if periodPrincipal.Cmp(balance) > 0 {
			return nil, fmt.Errorf("%w: rounding policy exhausts principal before final installment", loan.ErrInvariant)
		}
		balance = balance.Sub(periodPrincipal)
		rows[index] = loan.ContractualScheduleRow{
			Number: installment.Number, DueDate: installment.DueDate, Principal: periodPrincipal,
			Interest: interestPerPeriod, Installment: periodPrincipal.Add(interestPerPeriod), ScheduledBalance: balance,
		}
	}
	return rows, nil
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
