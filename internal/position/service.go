package position

import (
	"context"
	"errors"
	"fmt"
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
	if asOf.Before(service.cutoff) {
		position, err := service.mso.HistoricalPosition(ctx, primary, asOf)
		return loan.ResolvedPosition{Loan: contract, Position: position}, err
	}
	opening, err := service.mso.OpeningState(ctx, primary, service.cutoff)
	if err != nil {
		return loan.ResolvedPosition{}, err
	}
	if asOf.Equal(service.cutoff) {
		return loan.ResolvedPosition{Loan: contract, Position: openingPosition(opening, asOf)}, nil
	}
	if opening.InterestType != FlatInterestType || contract.ContractChanged || !validSchedule(contract) {
		position, err := service.exactPosition(ctx, primary, asOf, today)
		return loan.ResolvedPosition{Loan: contract, Position: position}, err
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
		FlatRatePercent: contract.FlatRatePercent, Opening: opening, DueDates: contract.DueDates,
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
	return loan.ResolvedPosition{Loan: contract, Position: position, Trace: calculation.Trace}, nil
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
	if contract.TenorMonths <= 0 || len(contract.DueDates) != contract.TenorMonths {
		return false
	}
	seen := make(map[string]struct{}, len(contract.DueDates))
	for _, date := range contract.DueDates {
		if date.IsZero() {
			return false
		}
		key := date.String()
		if _, duplicate := seen[key]; duplicate {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}
