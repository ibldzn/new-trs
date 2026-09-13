package reporting

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/loan"
)

var (
	ErrJobNotFound = errors.New("reporting job not found")
	ErrJobNotReady = errors.New("reporting job not ready")
)

type PositionService interface {
	GetLoanPosition(context.Context, string, loan.Date) (loan.ResolvedPosition, error)
}

type Row struct {
	AccountNumber  string
	Outstanding    string
	Collectability string
	Status         string
}

type Job struct {
	ID          string
	OwnerID     uint64
	AsOf        loan.Date
	Status      string
	Total       int
	Processed   int
	Failed      int
	Rows        []Row
	CreatedAt   time.Time
	FinishedAt  *time.Time
	csv         []byte
	cancel      context.CancelFunc
	attribution audit.Attribution
}

type Manager struct {
	mu          sync.Mutex
	jobs        map[string]*Job
	root        context.Context
	positions   PositionService
	concurrency int
	appendAudit func(context.Context, audit.Event) error
	logger      *slog.Logger
	cancel      context.CancelFunc
	wait        sync.WaitGroup
}

func NewManager(root context.Context, positions PositionService, concurrency int, appendAudit func(context.Context, audit.Event) error, logger *slog.Logger) (*Manager, error) {
	if root == nil || positions == nil || concurrency <= 0 {
		return nil, fmt.Errorf("reporting manager dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	managerContext, cancel := context.WithCancel(root)
	return &Manager{jobs: make(map[string]*Job), root: managerContext, cancel: cancel, positions: positions, concurrency: concurrency, appendAudit: appendAudit, logger: logger}, nil
}

func (manager *Manager) Submit(ownerID uint64, accounts []string, asOf loan.Date, attribution audit.Attribution) (Job, error) {
	if ownerID == 0 || len(accounts) == 0 || asOf.IsZero() {
		return Job{}, fmt.Errorf("%w: owner, accounts, and reporting date are required", loan.ErrInvalidInput)
	}
	if len(accounts) > 10000 {
		return Job{}, fmt.Errorf("%w: report supports at most 10000 rows", loan.ErrInvalidInput)
	}
	id, err := jobID()
	if err != nil {
		return Job{}, err
	}
	ctx, cancel := context.WithCancel(manager.root)
	job := &Job{ID: id, OwnerID: ownerID, AsOf: asOf, Status: "pending", Total: len(accounts), Rows: make([]Row, len(accounts)), CreatedAt: time.Now().UTC(), cancel: cancel, attribution: attribution}
	manager.mu.Lock()
	// ponytail: in-memory jobs; persist when restart durability becomes required.
	for existingID, existing := range manager.jobs {
		if existing.FinishedAt != nil && time.Since(*existing.FinishedAt) > time.Hour {
			delete(manager.jobs, existingID)
		}
	}
	manager.jobs[id] = job
	created := copyJob(job)
	manager.mu.Unlock()
	manager.wait.Add(1)
	go func() {
		defer manager.wait.Done()
		manager.run(ctx, job, append([]string(nil), accounts...))
	}()
	return created, nil
}

func (manager *Manager) Close() {
	manager.cancel()
	manager.wait.Wait()
}

func (manager *Manager) Get(id string, ownerID uint64, admin bool) (Job, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	job, ok := manager.jobs[id]
	if !ok || (!admin && job.OwnerID != ownerID) {
		return Job{}, ErrJobNotFound
	}
	return copyJob(job), nil
}

func (manager *Manager) Download(id string, ownerID uint64, admin bool) ([]byte, Job, error) {
	job, err := manager.Get(id, ownerID, admin)
	if err != nil {
		return nil, Job{}, err
	}
	if job.Status != "succeeded" {
		return nil, job, ErrJobNotReady
	}
	return append([]byte(nil), job.csv...), job, nil
}

func (manager *Manager) Cancel(id string, ownerID uint64, admin bool) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	job, ok := manager.jobs[id]
	if !ok || (!admin && job.OwnerID != ownerID) {
		return ErrJobNotFound
	}
	if job.cancel != nil && (job.Status == "pending" || job.Status == "running") {
		job.cancel()
	}
	return nil
}

func (manager *Manager) run(ctx context.Context, job *Job, accounts []string) {
	manager.mu.Lock()
	job.Status = "running"
	manager.mu.Unlock()
	work := make(chan int)
	var wait sync.WaitGroup
	workers := min(manager.concurrency, len(accounts))
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range work {
				resolved, err := manager.positions.GetLoanPosition(ctx, accounts[index], job.AsOf)
				row := Row{AccountNumber: accounts[index], Status: "ok"}
				if err != nil {
					row.Status = failureStatus(err)
				} else {
					row.Outstanding = resolved.Position.PrincipalOutstanding.Format(2)
					row.Collectability = strconv.Itoa(resolved.Position.CollectabilityBI)
				}
				manager.mu.Lock()
				job.Rows[index] = row
				job.Processed++
				if err != nil {
					job.Failed++
				}
				manager.mu.Unlock()
			}
		}()
	}
sendLoop:
	for index := range accounts {
		select {
		case work <- index:
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(work)
	wait.Wait()
	manager.mu.Lock()
	finished := time.Now().UTC()
	job.FinishedAt = &finished
	if ctx.Err() != nil {
		job.Status = "canceled"
		manager.mu.Unlock()
		return
	}
	encoded, err := encodeCSV(job.Rows)
	if err != nil {
		job.Status = "failed"
		manager.mu.Unlock()
		manager.logger.Error("encode reporting CSV", "job_id", job.ID, "error", err)
		return
	}
	job.csv = encoded
	job.Status = "succeeded"
	failed := job.Failed
	rowCount := job.Total
	attribution := job.attribution
	asOf := job.AsOf.String()
	manager.mu.Unlock()
	if manager.appendAudit != nil {
		if err := manager.appendAudit(ctx, audit.Event{Attribution: attribution, Action: audit.ActionReportingGenerate, Metadata: audit.ReportingMetadata{AsOf: asOf, RowCount: rowCount, Failed: failed}, CreatedAt: finished}); err != nil {
			manager.logger.WarnContext(ctx, "append reporting audit", "job_id", job.ID, "error", err)
		}
	}
}

func encodeCSV(rows []Row) ([]byte, error) {
	var output bytes.Buffer
	writer := csv.NewWriter(&output)
	if err := writer.Write([]string{"account_number", "loan_outstanding", "bi_collectability", "status"}); err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := writer.Write([]string{row.AccountNumber, row.Outstanding, row.Collectability, row.Status}); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	return output.Bytes(), writer.Error()
}

func failureStatus(err error) string {
	switch {
	case errors.Is(err, loan.ErrInvalidInput):
		return "error: invalid input"
	case errors.Is(err, loan.ErrUnsupportedCalculation):
		return "error: unsupported calculation"
	case errors.Is(err, loan.ErrAmbiguousAccountResolution):
		return "error: ambiguous account resolution"
	case errors.Is(err, loan.ErrCurrentSnapshot):
		return "error: current snapshot unavailable"
	case errors.Is(err, loan.ErrHistoricalEvidence), errors.Is(err, loan.ErrDWHUnavailable), errors.Is(err, loan.ErrMSOUnavailable):
		return "error: historical evidence unavailable"
	case errors.Is(err, loan.ErrNotFound):
		return "error: loan not found"
	default:
		return "error: upstream unavailable"
	}
}

func copyJob(source *Job) Job {
	copy := *source
	copy.Rows = append([]Row(nil), source.Rows...)
	copy.csv = append([]byte(nil), source.csv...)
	copy.cancel = nil
	return copy
}

func jobID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate reporting job ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
