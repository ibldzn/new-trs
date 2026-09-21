package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/loan"
)

type source interface {
	FetchCurrentLoanPositions(context.Context, loan.Date) ([]loan.LoanPosition, error)
}

type store interface {
	ReplaceAll(context.Context, []loan.LoanPosition, time.Time) error
	Status(context.Context) (Status, error)
	MarkAttempt(context.Context, time.Time) error
	MarkFailure(context.Context, time.Time, string) error
	AcquireRefreshLock(context.Context) (func(), bool, error)
}

type Service struct {
	source      source
	store       store
	location    *time.Location
	now         func() time.Time
	appendAudit func(context.Context, audit.Event) error
	logger      *slog.Logger
}

func NewService(source source, store store, location *time.Location, appendAudit func(context.Context, audit.Event) error, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{source: source, store: store, location: location, now: time.Now, appendAudit: appendAudit, logger: logger}
}

func (service *Service) Status(ctx context.Context) (Status, error) { return service.store.Status(ctx) }

func (service *Service) Refresh(ctx context.Context, trigger Trigger, attribution audit.Attribution) (int, error) {
	release, acquired, err := service.store.AcquireRefreshLock(ctx)
	if err != nil {
		return 0, err
	}
	if !acquired {
		return 0, ErrRefreshInProgress
	}
	defer release()
	now := service.now()
	if err := service.store.MarkAttempt(ctx, now); err != nil {
		return 0, fmt.Errorf("record snapshot attempt: %w", err)
	}
	service.audit(ctx, audit.Event{Attribution: attribution, Action: audit.ActionSnapshotRefreshStarted, Metadata: audit.SnapshotRefreshMetadata{Trigger: string(trigger)}, CreatedAt: now})
	businessDate := loan.NewDate(now, service.location)
	rows, err := service.source.FetchCurrentLoanPositions(ctx, businessDate)
	if err == nil {
		err = validateRows(rows, businessDate)
	}
	if err == nil {
		err = service.store.ReplaceAll(ctx, rows, service.now())
	}
	if err != nil {
		summary := safeSummary(err)
		failureContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = service.store.MarkFailure(failureContext, service.now(), summary)
		service.audit(failureContext, audit.Event{Attribution: attribution, Action: audit.ActionSnapshotRefreshFailed, Metadata: audit.SnapshotRefreshMetadata{Trigger: string(trigger), Error: summary}, CreatedAt: service.now()})
		cancel()
		service.logger.ErrorContext(ctx, "current snapshot refresh failed", "trigger", trigger, "error", err)
		return 0, err
	}
	service.audit(ctx, audit.Event{Attribution: attribution, Action: audit.ActionSnapshotRefreshSucceeded, Metadata: audit.SnapshotRefreshMetadata{Trigger: string(trigger), RowCount: len(rows)}, CreatedAt: service.now()})
	service.logger.InfoContext(ctx, "current snapshot refreshed", "trigger", trigger, "row_count", len(rows), "as_of", businessDate.String())
	return len(rows), nil
}

func validateRows(rows []loan.LoanPosition, date loan.Date) error {
	if len(rows) == 0 {
		return fmt.Errorf("snapshot source returned no rows")
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		account := strings.TrimSpace(row.AccountNumber)
		if account == "" || !row.AsOf.Equal(date) || row.LoanStartDate.IsZero() || row.LoanStartDate.After(date) || row.PrincipalOutstanding.IsNegative() || row.PrincipalDue.IsNegative() || row.InterestDue.IsNegative() || row.PenaltyDue.IsNegative() || row.PrincipalDue.Cmp(row.PrincipalOutstanding) > 0 || row.CollectabilityBI < 1 || row.CollectabilityBI > 5 {
			return fmt.Errorf("invalid snapshot row for account %q", account)
		}
		if _, duplicate := seen[account]; duplicate {
			return fmt.Errorf("duplicate snapshot account %q", account)
		}
		seen[account] = struct{}{}
	}
	return nil
}

func (service *Service) audit(ctx context.Context, event audit.Event) {
	if service.appendAudit == nil {
		return
	}
	if err := service.appendAudit(ctx, event); err != nil {
		service.logger.WarnContext(ctx, "append snapshot audit", "action", event.Action, "error", err)
	}
}

func safeSummary(err error) string {
	summary := strings.Join(strings.Fields(err.Error()), " ")
	if len(summary) > 500 {
		summary = summary[:500]
	}
	return summary
}

func IsUnavailable(err error) bool {
	return errors.Is(err, loan.ErrCurrentSnapshot)
}
