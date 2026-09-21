package snapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/loan"
)

type sourceFake struct {
	rows    []loan.LoanPosition
	err     error
	started chan struct{}
	release chan struct{}
	order   *[]string
}

func (source *sourceFake) FetchCurrentLoanPositions(context.Context, loan.Date) ([]loan.LoanPosition, error) {
	if source.order != nil {
		*source.order = append(*source.order, "fetch")
	}
	if source.started != nil {
		close(source.started)
		<-source.release
	}
	return append([]loan.LoanPosition(nil), source.rows...), source.err
}

type storeFake struct {
	mu       sync.Mutex
	rows     []loan.LoanPosition
	status   Status
	locked   bool
	order    *[]string
	replaced int
}

func (store *storeFake) ReplaceAll(_ context.Context, rows []loan.LoanPosition, _ time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.order != nil {
		*store.order = append(*store.order, "replace")
	}
	store.rows = append([]loan.LoanPosition(nil), rows...)
	store.replaced++
	now := time.Now()
	store.status.LastSuccessfulAt = &now
	store.status.LastStatus = "succeeded"
	store.status.LastRowCount = int64(len(rows))
	store.status.LastErrorSummary = nil
	return nil
}
func (store *storeFake) Status(context.Context) (Status, error) { return store.status, nil }
func (store *storeFake) MarkAttempt(_ context.Context, now time.Time) error {
	store.status.LastAttemptedAt = &now
	store.status.LastStatus = "running"
	return nil
}
func (store *storeFake) MarkFailure(_ context.Context, _ time.Time, summary string) error {
	store.status.LastStatus = "failed"
	store.status.LastErrorSummary = &summary
	return nil
}
func (store *storeFake) AcquireRefreshLock(context.Context) (func(), bool, error) {
	store.mu.Lock()
	if store.locked {
		store.mu.Unlock()
		return nil, false, nil
	}
	store.locked = true
	store.mu.Unlock()
	return func() { store.mu.Lock(); store.locked = false; store.mu.Unlock() }, true, nil
}

func TestRefreshFetchesBeforeFullReplacementAndUpdatesStatus(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	now := time.Date(2026, 9, 13, 18, 30, 0, 0, time.UTC)
	today := loan.NewDate(now, location)
	old := positionRow(today, "old", "10")
	newRow := positionRow(today, "new", "20")
	order := []string{}
	store := &storeFake{rows: []loan.LoanPosition{old}, order: &order}
	service := NewService(&sourceFake{rows: []loan.LoanPosition{newRow}, order: &order}, store, location, nil, nil)
	service.now = func() time.Time { return now }
	count, err := service.Refresh(context.Background(), TriggerManual, auditAttribution())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(store.rows) != 1 || store.rows[0].AccountNumber != "new" || len(order) != 2 || order[0] != "fetch" || order[1] != "replace" {
		t.Fatalf("count=%d rows=%+v order=%v", count, store.rows, order)
	}
	if store.status.LastStatus != "succeeded" || store.status.LastRowCount != 1 || store.status.LastSuccessfulAt == nil || !store.rows[0].AsOf.Equal(today) || today.String() != "2026-09-14" {
		t.Fatalf("status=%+v today=%s", store.status, today)
	}
}

func TestRefreshFailurePreservesPreviousSnapshot(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	now := time.Date(2026, 9, 14, 1, 0, 0, 0, location)
	today := loan.NewDate(now, location)
	negativePenalty := positionRow(today, "new", "10")
	negativePenalty.PenaltyDue = loan.MustMoney("-1")
	for _, test := range []struct {
		name   string
		source *sourceFake
	}{
		{"fetch", &sourceFake{err: errors.New("upstream down")}},
		{"validation", &sourceFake{rows: []loan.LoanPosition{positionRow(today, "same", "10"), positionRow(today, "same", "10")}}},
		{"missing loan start date", &sourceFake{rows: []loan.LoanPosition{{AsOf: today, AccountNumber: "new", PrincipalOutstanding: loan.MustMoney("10"), CollectabilityBI: 1}}}},
		{"negative penalty arrears", &sourceFake{rows: []loan.LoanPosition{negativePenalty}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &storeFake{rows: []loan.LoanPosition{positionRow(today, "old", "9")}}
			service := NewService(test.source, store, location, nil, nil)
			service.now = func() time.Time { return now }
			if _, err := service.Refresh(context.Background(), TriggerScheduled, auditAttribution()); err == nil {
				t.Fatal("expected error")
			}
			if store.replaced != 0 || len(store.rows) != 1 || store.rows[0].AccountNumber != "old" || store.status.LastStatus != "failed" || store.status.LastErrorSummary == nil {
				t.Fatalf("store=%+v", store)
			}
		})
	}
}

func TestRefreshCannotOverlap(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	now := time.Date(2026, 9, 14, 1, 0, 0, 0, location)
	today := loan.NewDate(now, location)
	source := &sourceFake{rows: []loan.LoanPosition{positionRow(today, "one", "10")}, started: make(chan struct{}), release: make(chan struct{})}
	store := &storeFake{}
	service := NewService(source, store, location, nil, nil)
	service.now = func() time.Time { return now }
	done := make(chan error)
	go func() {
		_, err := service.Refresh(context.Background(), TriggerScheduled, auditAttribution())
		done <- err
	}()
	<-source.started
	if _, err := service.Refresh(context.Background(), TriggerManual, auditAttribution()); !errors.Is(err, ErrRefreshInProgress) {
		t.Fatalf("error = %v", err)
	}
	close(source.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func positionRow(date loan.Date, account, principal string) loan.LoanPosition {
	start, _ := loan.ParseDate("2024-06-01", time.UTC)
	return loan.LoanPosition{AsOf: date, LoanStartDate: start, AccountNumber: account, PrincipalOutstanding: loan.MustMoney(principal), CollectabilityBI: 1}
}

func auditAttribution() audit.Attribution { return audit.Attribution{} }
