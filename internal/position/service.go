package position

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

const FlatInterestType = "10"

type FincloudGateway interface {
	ResolveLoan(context.Context, string, *time.Location) (loan.ContractData, error)
}

type MSORepository interface {
	HistoricalPosition(context.Context, string, loan.Date) (loan.LoanPosition, error)
	OpeningState(context.Context, string, loan.Date) (loan.OpeningLoanState, error)
}

type DWHRepository interface {
	ExactPosition(context.Context, string, loan.Date) (loan.LoanPosition, error)
	CollectabilityTimeline(context.Context, string, loan.Date, loan.Date) ([]loan.CollectabilityPoint, error)
}

type SnapshotRepository interface {
	ExactPosition(context.Context, string, loan.Date) (loan.LoanPosition, error)
}

type Calculator interface {
	Calculate(loan.CalculationInput) (loan.CalculationResult, error)
}

type Service struct {
	fincloud   FincloudGateway
	mso        MSORepository
	dwh        DWHRepository
	snapshot   SnapshotRepository
	calculator Calculator
	location   *time.Location
	cutoff     loan.Date
	now        func() time.Time
}

func NewService(fincloud FincloudGateway, mso MSORepository, dwh DWHRepository, snapshot SnapshotRepository, calculator Calculator, location *time.Location) (*Service, error) {
	if fincloud == nil || mso == nil || dwh == nil || snapshot == nil || calculator == nil || location == nil {
		return nil, fmt.Errorf("position service dependencies are required")
	}
	cutoff, err := loan.ParseDate("2025-10-12", location)
	if err != nil {
		return nil, err
	}
	return &Service{fincloud: fincloud, mso: mso, dwh: dwh, snapshot: snapshot, calculator: calculator, location: location, cutoff: cutoff, now: time.Now}, nil
}

func (service *Service) GetLoanPosition(ctx context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
	account = strings.TrimSpace(account)
	if account == "" || asOf.IsZero() {
		return loan.ResolvedPosition{}, fmt.Errorf("%w: account and as-of date are required", loan.ErrInvalidInput)
	}
	today := loan.NewDate(service.now(), service.location)
	if asOf.After(today) {
		return loan.ResolvedPosition{}, fmt.Errorf("%w: future as-of dates are not supported", loan.ErrInvalidInput)
	}
	contract, err := service.fincloud.ResolveLoan(ctx, account, service.location)
	if err != nil {
		return loan.ResolvedPosition{}, err
	}
	primary := strings.TrimSpace(contract.PrimaryAccount)
	if primary == "" {
		return loan.ResolvedPosition{}, fmt.Errorf("%w: resolved primary account is empty", loan.ErrInvariant)
	}
	if !contract.CloseDate.IsZero() && !contract.CloseDate.After(asOf) {
		logDecision(ctx, contract, "", loan.SourceClosed, "closed_as_of")
		return loan.ResolvedPosition{Loan: contract, Position: loan.LoanPosition{
			AsOf: asOf, AccountNumber: primary, CollectabilityBI: contract.CurrentCollectability, Source: loan.SourceClosed,
		}}, nil
	}
	if !contract.DisbursementDate.IsZero() && contract.DisbursementDate.After(service.cutoff) && asOf.Before(contract.DisbursementDate) {
		return loan.ResolvedPosition{}, fmt.Errorf("%w: reporting date precedes Fincloud-native disbursement", loan.ErrHistoricalEvidence)
	}
	if !asOf.Before(service.cutoff) {
		if contract.DisbursementDate.IsZero() {
			return loan.ResolvedPosition{}, fmt.Errorf("%w: Fincloud disbursement date is missing", loan.ErrHistoricalEvidence)
		}
		if contract.DisbursementDate.After(service.cutoff) {
			position, err := service.exactPosition(ctx, primary, asOf, today)
			selected, reason := loan.SourceDWH, "fincloud_native_historical"
			if asOf.Equal(today) {
				selected, reason = loan.SourceTodaySnapshot, "fincloud_native_today"
			}
			logDecision(ctx, contract, "", selected, reason)
			return loan.ResolvedPosition{Loan: contract, Position: position}, err
		}
	}
	alternate := strings.TrimSpace(contract.AlternateAccount)
	if alternate == "" {
		return loan.ResolvedPosition{}, fmt.Errorf("%w: resolved alternate MSO account is empty", loan.ErrHistoricalEvidence)
	}
	msoAccount := formatFincloudAltNoToMSO(alternate)
	if asOf.Before(service.cutoff) {
		position, err := service.mso.HistoricalPosition(ctx, msoAccount, asOf)
		if err == nil {
			position.AccountNumber = primary
		}
		logDecision(ctx, contract, "", loan.SourceMSO, "before_cutoff")
		return loan.ResolvedPosition{Loan: contract, Position: position}, err
	}
	opening, err := service.mso.OpeningState(ctx, msoAccount, service.cutoff)
	if err != nil {
		return loan.ResolvedPosition{}, err
	}
	opening.AccountNumber = primary
	if asOf.Equal(service.cutoff) {
		logDecision(ctx, contract, opening.InterestType, loan.SourceMSO, "cutoff_opening")
		return loan.ResolvedPosition{Loan: contract, Position: openingPosition(opening, asOf)}, nil
	}
	if opening.InterestType != FlatInterestType {
		position, err := service.exactPosition(ctx, primary, asOf, today)
		selected := loan.SourceDWH
		if asOf.Equal(today) {
			selected = loan.SourceTodaySnapshot
		}
		logDecision(ctx, contract, opening.InterestType, selected, "non_flat_interest_type")
		return loan.ResolvedPosition{Loan: contract, Position: position}, err
	}
	if contract.ContractScheduleEvidence != nil {
		contract.ContractSchedule, err = normalizeContractSchedule(contract.ContractScheduleEvidence, contract.TenorMonths, service.location)
		if err != nil {
			logDecision(ctx, contract, opening.InterestType, "", "invalid_contractual_schedule")
			return loan.ResolvedPosition{}, err
		}
	}
	if !validSchedule(contract) {
		logDecision(ctx, contract, opening.InterestType, "", "invalid_contractual_schedule")
		return loan.ResolvedPosition{}, fmt.Errorf("%w: invalid contractual installment schedule", loan.ErrUnsupportedCalculation)
	}

	timelineEnd := asOf
	if asOf.Equal(today) {
		timelineEnd = today.AddDays(-1, service.location)
	}
	timeline, err := service.dwh.CollectabilityTimeline(ctx, primary, service.cutoff.AddDays(1, service.location), timelineEnd)
	if err != nil {
		return loan.ResolvedPosition{}, err
	}
	if asOf.Equal(today) {
		current, err := service.snapshot.ExactPosition(ctx, primary, today)
		if err != nil {
			if errors.Is(err, loan.ErrNotFound) {
				return loan.ResolvedPosition{}, errors.Join(loan.ErrCurrentSnapshot, err)
			}
			return loan.ResolvedPosition{}, err
		}
		timeline = append(timeline, loan.CollectabilityPoint{Date: today, Value: current.CollectabilityBI})
	}
	calculation, err := service.calculator.Calculate(loan.CalculationInput{
		AsOf: asOf, Cutoff: service.cutoff, ContractualPrincipal: contract.PlafondLimit, TenorMonths: contract.TenorMonths,
		FlatRatePercent: contract.FlatRatePercent, Opening: opening, ContractSchedule: contract.ContractSchedule,
		Repayments: contract.Repayments, CollectabilityTimeline: timeline,
	})
	if err != nil {
		return loan.ResolvedPosition{}, err
	}
	position := loan.LoanPosition{
		AsOf: asOf, AccountNumber: primary, PrincipalOutstanding: calculation.PrincipalOutstanding,
		PrincipalDue: calculation.PrincipalDue, InterestDue: calculation.InterestDue,
		CollectabilityBI: calculation.CollectabilityBI, UnappliedAmount: calculation.UnappliedAmount, Source: loan.SourceReconstructed,
	}
	logDecision(ctx, contract, opening.InterestType, loan.SourceReconstructed, "reconstructed")
	return loan.ResolvedPosition{Loan: contract, Position: position, Trace: calculation.Trace, ContractualSchedule: calculation.ContractualSchedule}, nil
}

func formatFincloudAltNoToMSO(account string) string {
	account = strings.TrimSpace(account)
	if len(account) != 10 {
		return account
	}
	return account[:2] + "." + account[2:5] + "." + account[5:]
}

func (service *Service) exactPosition(ctx context.Context, account string, asOf, today loan.Date) (loan.LoanPosition, error) {
	if asOf.Equal(today) {
		return service.snapshot.ExactPosition(ctx, account, asOf)
	}
	return service.dwh.ExactPosition(ctx, account, asOf)
}

func openingPosition(opening loan.OpeningLoanState, asOf loan.Date) loan.LoanPosition {
	return loan.LoanPosition{
		AsOf: asOf, AccountNumber: opening.AccountNumber, PrincipalOutstanding: opening.PrincipalOutstanding,
		PrincipalDue: opening.PrincipalDue, InterestDue: opening.InterestDue,
		CollectabilityBI: opening.CollectabilityBI, Source: loan.SourceMSO,
	}
}

func validSchedule(contract loan.ContractData) bool {
	if contract.TenorMonths <= 0 || len(contract.ContractSchedule) != contract.TenorMonths {
		return false
	}
	for index, installment := range contract.ContractSchedule {
		if installment.Number != index+1 || installment.DueDate.IsZero() {
			return false
		}
		if index > 0 && !installment.DueDate.After(contract.ContractSchedule[index-1].DueDate) {
			return false
		}
	}
	return true
}

func normalizeContractSchedule(source []loan.ContractualInstallmentEvidence, tenor int, location *time.Location) ([]loan.ContractualInstallment, error) {
	schedule := make([]loan.ContractualInstallment, 0, len(source))
	seen := make(map[int64]struct{}, len(source))
	for _, row := range source {
		if row.Number < 0 || row.Number > int64(tenor) {
			return nil, fmt.Errorf("%w: installment number %d outside 0..%d", loan.ErrUnsupportedCalculation, row.Number, tenor)
		}
		if row.Number == 0 {
			continue
		}
		if _, duplicate := seen[row.Number]; duplicate {
			return nil, fmt.Errorf("%w: duplicate contractual installment %d", loan.ErrUnsupportedCalculation, row.Number)
		}
		rawDate := strings.TrimSpace(row.RawDueDate)
		if len(rawDate) >= len(loan.DateLayout) {
			rawDate = rawDate[:len(loan.DateLayout)]
		}
		date, err := loan.ParseDate(rawDate, location)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid date for contractual installment %d: %v", loan.ErrUnsupportedCalculation, row.Number, err)
		}
		seen[row.Number] = struct{}{}
		schedule = append(schedule, loan.ContractualInstallment{Number: int(row.Number), DueDate: date})
	}
	slices.SortFunc(schedule, func(left, right loan.ContractualInstallment) int { return cmp.Compare(left.Number, right.Number) })
	if len(schedule) != tenor {
		return nil, fmt.Errorf("%w: got %d contractual installments for %d-month tenor", loan.ErrUnsupportedCalculation, len(schedule), tenor)
	}
	for index, installment := range schedule {
		if installment.Number != index+1 {
			return nil, fmt.Errorf("%w: missing contractual installment %d", loan.ErrUnsupportedCalculation, index+1)
		}
		if index > 0 && !installment.DueDate.After(schedule[index-1].DueDate) {
			return nil, fmt.Errorf("%w: contractual installment dates are not strictly chronological", loan.ErrUnsupportedCalculation)
		}
	}
	return schedule, nil
}

func logDecision(ctx context.Context, contract loan.ContractData, interestType string, source loan.PositionSource, reason string) {
	slog.InfoContext(ctx, "loan position source selected",
		"primary_account", contract.PrimaryAccount,
		"alternate_account", contract.AlternateAccount,
		"mso_interest_type", interestType,
		"tenor_months", contract.TenorMonths,
		"raw_fincloud_schedule_rows", contract.RawScheduleCount,
		"normalized_contractual_schedule_count", len(contract.ContractSchedule),
		"selected_position_source", source,
		"reason", reason,
	)
}
