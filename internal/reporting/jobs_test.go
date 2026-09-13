package reporting

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/loan"
)

type positionFake struct {
	delays  map[string]time.Duration
	errors  map[string]error
	started chan struct{}
	once    sync.Once
}

func (fake *positionFake) GetLoanPosition(ctx context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
	if fake.started != nil {
		fake.once.Do(func() { close(fake.started) })
	}
	if delay := fake.delays[account]; delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return loan.ResolvedPosition{}, ctx.Err()
		}
	}
	if err := fake.errors[account]; err != nil {
		return loan.ResolvedPosition{}, err
	}
	return loan.ResolvedPosition{Position: loan.LoanPosition{AccountNumber: "primary-" + account, AsOf: asOf, PrincipalOutstanding: loan.MustMoney("12.34"), CollectabilityBI: 2}}, nil
}

func TestManagerBoundsWorkPreservesOrderAndExposesFailures(t *testing.T) {
	location := time.UTC
	asOf, _ := loan.ParseDate("2026-09-14", location)
	auditDone := make(chan struct{}, 1)
	manager, err := NewManager(context.Background(), &positionFake{
		delays: map[string]time.Duration{"first": 20 * time.Millisecond}, errors: map[string]error{"bad": loan.ErrHistoricalEvidence},
	}, 2, func(context.Context, audit.Event) error { auditDone <- struct{}{}; return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	job, err := manager.Submit(7, []string{"first", "bad", "last"}, asOf, audit.Attribution{})
	if err != nil {
		t.Fatal(err)
	}
	job = waitJob(t, manager, job.ID, 7)
	select {
	case <-auditDone:
	case <-time.After(time.Second):
		t.Fatal("report audit timeout")
	}
	if job.Status != "succeeded" || job.Processed != 3 || job.Failed != 1 {
		t.Fatalf("job=%+v", job)
	}
	body, _, err := manager.Download(job.ID, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	reader := csv.NewReader(strings.NewReader(string(body)))
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 4 || records[1][0] != "first" || records[2][0] != "bad" || records[2][1] != "" || records[2][2] != "" || records[2][3] != "error: historical evidence unavailable" || records[3][0] != "last" {
		t.Fatalf("records = %v", records)
	}
	if _, err := manager.Get(job.ID, 8, false); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("cross-owner error = %v", err)
	}
}

func TestManagerCancellationStopsPendingWork(t *testing.T) {
	asOf, _ := loan.ParseDate("2026-09-14", time.UTC)
	fake := &positionFake{delays: map[string]time.Duration{"one": time.Second, "two": time.Second}, started: make(chan struct{})}
	manager, _ := NewManager(context.Background(), fake, 1, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	job, _ := manager.Submit(7, []string{"one", "two"}, asOf, audit.Attribution{})
	<-fake.started
	if err := manager.Cancel(job.ID, 7, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		job, _ = manager.Get(job.ID, 7, false)
		if job.Status == "canceled" {
			if _, _, err := manager.Download(job.ID, 7, false); !errors.Is(err, ErrJobNotReady) {
				t.Fatalf("download error = %v", err)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job did not cancel: %+v", job)
}

func waitJob(t *testing.T, manager *Manager, id string, owner uint64) Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := manager.Get(id, owner, false)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == "succeeded" || job.Status == "failed" || job.Status == "canceled" {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job timeout")
	return Job{}
}
