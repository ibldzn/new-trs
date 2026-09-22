package slik

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/contractual"
	"github.com/ibldzn/trs/internal/loan"
)

type positionFunc func(context.Context, string, loan.Date) (loan.ResolvedPosition, error)

func (function positionFunc) GetLoanPosition(ctx context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
	return function(ctx, account, asOf)
}

func syntheticReversalPositions(rows []loan.Repayment) positionFunc {
	return func(_ context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
		cutoff, _ := loan.ParseDate("2025-10-12", time.UTC)
		result, err := (contractual.Calculator{}).Calculate(loan.CalculationInput{
			AsOf: asOf, Cutoff: cutoff, ContractualPrincipal: loan.MustMoney("100"), TenorMonths: 1,
			FlatRatePercent:  loan.MustMoney("18"),
			Opening:          loan.OpeningLoanState{PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 1},
			ContractSchedule: []loan.ContractualInstallment{{Number: 1, DueDate: asOf}}, Repayments: rows,
		})
		if err != nil {
			return loan.ResolvedPosition{}, err
		}
		return loan.ResolvedPosition{
			Loan:     loan.ContractData{PrimaryAccount: account, FlatRatePercent: loan.MustMoney("18"), Repayments: rows},
			Position: loan.LoanPosition{AsOf: asOf, AccountNumber: account, PrincipalOutstanding: result.PrincipalOutstanding, Source: loan.SourceReconstructed},
		}, nil
	}
}

type memoryStore struct {
	mu        sync.Mutex
	lock      sync.Mutex
	jobs      map[string]Job
	accounts  map[string]map[string]Values
	recovered chan struct{}
	lockLost  chan struct{}
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: make(map[string]Job), accounts: make(map[string]map[string]Values), recovered: make(chan struct{}, 1)}
}
func (store *memoryStore) Create(_ context.Context, job Job, accounts []string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.jobs[job.ID] = job
	store.accounts[job.ID] = make(map[string]Values)
	for _, account := range accounts {
		store.accounts[job.ID][account] = Values{}
	}
	return nil
}
func (store *memoryStore) Get(_ context.Context, id string, owner uint64, viewAll bool) (Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[id]
	if !ok || (!viewAll && job.OwnerID != owner) {
		return Job{}, ErrNotFound
	}
	return job, nil
}
func (store *memoryStore) History(_ context.Context, owner uint64, viewAll bool) ([]Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var jobs []Job
	for _, job := range store.jobs {
		if viewAll || job.OwnerID == owner {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}
func (store *memoryStore) Recover(context.Context) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for id, job := range store.jobs {
		if job.Status == "PROCESSING" {
			job.Status = "QUEUED"
			store.jobs[id] = job
		}
	}
	select {
	case store.recovered <- struct{}{}:
	default:
	}
	return nil
}
func (store *memoryStore) ClaimNext(context.Context) (Job, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var ids []string
	for id, job := range store.jobs {
		if job.Status == "QUEUED" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return Job{}, false, nil
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := store.jobs[ids[i]], store.jobs[ids[j]]
		if a.CreatedAt.Equal(b.CreatedAt) {
			return a.ID < b.ID
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	job := store.jobs[ids[0]]
	job.Status = "PROCESSING"
	now := time.Now()
	if job.StartedAt == nil {
		job.StartedAt = &now
	}
	store.jobs[job.ID] = job
	return job, true, nil
}
func (store *memoryStore) Pending(_ context.Context, id, after string, limit int) ([]string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var accounts []string
	for account, value := range store.accounts[id] {
		if account > after && value.Balance == "" {
			accounts = append(accounts, account)
		}
	}
	sort.Strings(accounts)
	return accounts[:min(limit, len(accounts))], nil
}
func (store *memoryStore) Save(_ context.Context, id, account, _ string, value Values) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.jobs[id].Status != "PROCESSING" {
		return errors.New("job not processing")
	}
	if store.accounts[id][account].Balance != "" {
		return errors.New("duplicate checkpoint")
	}
	store.accounts[id][account] = value
	job := store.jobs[id]
	job.Processed++
	store.jobs[id] = job
	return nil
}
func (store *memoryStore) Results(_ context.Context, id string) (map[string]Values, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make(map[string]Values)
	for account, value := range store.accounts[id] {
		if value.Balance != "" {
			result[account] = value
		}
	}
	return result, nil
}
func (store *memoryStore) Terminal(_ context.Context, id, status, account, reason, output string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	job := store.jobs[id]
	if job.Status != "PROCESSING" {
		return errors.New("job not processing")
	}
	job.Status = status
	job.FailedAccount = account
	job.FailureReason = reason
	job.OutputFile = output
	now := time.Now()
	expires := now.Add(7 * 24 * time.Hour)
	job.FinishedAt = &now
	job.ExpiresAt = &expires
	store.jobs[id] = job
	return nil
}
func (store *memoryStore) Cancel(_ context.Context, id string, owner uint64, viewAll bool) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	job, ok := store.jobs[id]
	if !ok || (!viewAll && job.OwnerID != owner) {
		return ErrNotFound
	}
	if job.Status == "QUEUED" || job.Status == "PROCESSING" {
		job.Status = "CANCELED"
		now := time.Now()
		expires := now.Add(7 * 24 * time.Hour)
		job.FinishedAt = &now
		job.ExpiresAt = &expires
		store.jobs[id] = job
	}
	return nil
}
func (store *memoryStore) IsCanceled(_ context.Context, id string) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.jobs[id].Status == "CANCELED", nil
}
func (store *memoryStore) Expired(context.Context) ([]Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var jobs []Job
	for _, job := range store.jobs {
		if job.ExpiresAt != nil && time.Now().After(*job.ExpiresAt) && (job.Status == "COMPLETED" || job.Status == "FAILED" || job.Status == "CANCELED") && (job.InputFile != "" || job.OutputFile != "") {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}
func (store *memoryStore) ClearFiles(_ context.Context, id string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	job := store.jobs[id]
	job.InputFile = ""
	job.OutputFile = ""
	store.jobs[id] = job
	return nil
}
func (store *memoryStore) AcquireLock(context.Context) (func(), <-chan struct{}, bool, error) {
	if !store.lock.TryLock() {
		return nil, nil, false, nil
	}
	store.mu.Lock()
	lost := store.lockLost
	store.mu.Unlock()
	if lost == nil {
		lost = make(chan struct{})
	}
	return store.lock.Unlock, lost, true, nil
}
func (store *memoryStore) CheckComplete(_ context.Context, id string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, value := range store.accounts[id] {
		if value.Balance == "" {
			return errors.New("remaining account")
		}
	}
	return nil
}

type controlledPositions struct {
	mu                        sync.Mutex
	active, maximum, canceled int
	calls                     map[string]int
	started                   chan string
	release                   <-chan struct{}
	failAccount               string
	failRelease               <-chan struct{}
	blockAccount              string
	lateErrorAccount          string
}

func (fake *controlledPositions) GetLoanPosition(ctx context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
	fake.mu.Lock()
	if fake.calls == nil {
		fake.calls = make(map[string]int)
	}
	fake.calls[account]++
	fake.active++
	fake.maximum = max(fake.maximum, fake.active)
	fake.mu.Unlock()
	defer func() { fake.mu.Lock(); fake.active--; fake.mu.Unlock() }()
	if fake.started != nil {
		fake.started <- account
	}
	if account == fake.failAccount {
		select {
		case <-fake.failRelease:
			return loan.ResolvedPosition{}, loan.ErrNotFound
		case <-ctx.Done():
			fake.mu.Lock()
			fake.canceled++
			fake.mu.Unlock()
			if account == fake.lateErrorAccount {
				return loan.ResolvedPosition{}, loan.ErrFincloudUnavailable
			}
			return loan.ResolvedPosition{}, ctx.Err()
		}
	}
	if fake.release != nil && (fake.blockAccount == "" || fake.blockAccount == account) {
		select {
		case <-fake.release:
		case <-ctx.Done():
			fake.mu.Lock()
			fake.canceled++
			fake.mu.Unlock()
			return loan.ResolvedPosition{}, ctx.Err()
		}
	}
	return loan.ResolvedPosition{Loan: loan.ContractData{PrimaryAccount: "primary-" + account, FlatRatePercent: loan.MustMoney("11.234567890123")}, Position: loan.LoanPosition{AsOf: asOf, PrincipalOutstanding: loan.MustMoney("1234.567")}}, nil
}
func (fake *controlledPositions) snapshot() (int, int, int, map[string]int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	calls := make(map[string]int)
	for k, v := range fake.calls {
		calls[k] = v
	}
	return fake.active, fake.maximum, fake.canceled, calls
}

func accountsWorkbook(t testing.TB, accounts []string) string {
	t.Helper()
	base := fixture(t, 1, "")
	source, err := zip.OpenReader(base)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	output := filepath.Join(t.TempDir(), "accounts.xlsx")
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	var rows strings.Builder
	rows.WriteString(`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c><c r="C1" t="s"><v>2</v></c></row>`)
	for index, account := range accounts {
		row := index + 2
		rows.WriteString(fmt.Sprintf(`<row r="%d"><c r="A%d" t="inlineStr"><is><t>%s</t></is></c><c r="B%d" s="2" t="s"><v>6</v></c><c r="C%d" s="2" t="s"><v>6</v></c></row>`, row, row, account, row, row))
	}
	for _, part := range source.File {
		data, err := readPart(part)
		if err != nil {
			t.Fatal(err)
		}
		if part.Name == "xl/worksheets/sheet1.xml" {
			start := bytesIndex(data, "<sheetData>")
			end := bytesIndex(data, "</sheetData>")
			data = []byte(string(data[:start]) + "<sheetData>" + rows.String() + string(data[end:]))
		}
		writer, err := archive.Create(part.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return output
}
func bytesIndex(data []byte, marker string) int { return strings.Index(string(data), marker) }

func testManager(t *testing.T, store *memoryStore, positions PositionService, concurrency int, directory string) *Manager {
	t.Helper()
	manager, err := NewManager(context.Background(), store, positions, Config{Concurrency: concurrency, MaxUploadBytes: 1 << 20, StorageDir: directory}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager
}
func submitWorkbook(t *testing.T, manager *Manager, path string) Job {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	date, _ := loan.ParseDate("2026-08-31", time.UTC)
	identity := audit.Identity{UserID: 7, Username: "test"}
	job, err := manager.Submit(context.Background(), audit.Attribution{Actor: &identity, Effective: &identity}, "input.xlsx", date, file)
	if err != nil {
		t.Fatal(err)
	}
	return job
}
func waitJobStatus(t *testing.T, manager *Manager, id, status string) Job {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		job, err := manager.Get(context.Background(), id, 7, false)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == status {
			return job
		}
		select {
		case <-deadline:
			t.Fatalf("job status %s, want %s", job.Status, status)
		case <-time.After(time.Millisecond):
		}
	}
}
func awaitStarted(t *testing.T, started <-chan string, count int) []string {
	t.Helper()
	var accounts []string
	for len(accounts) < count {
		select {
		case account := <-started:
			accounts = append(accounts, account)
		case <-time.After(5 * time.Second):
			t.Fatalf("started %v, want %d", accounts, count)
		}
	}
	return accounts
}

func TestBoundedWorkersAndDedup(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	fake := &controlledPositions{started: make(chan string, 20), release: release}
	manager := testManager(t, store, fake, 3, t.TempDir())
	job := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A", " A ", "B", "C", "D", "E", "A"}))
	started := awaitStarted(t, fake.started, 3)
	if len(started) != 3 {
		t.Fatal(started)
	}
	active, maximum, _, _ := fake.snapshot()
	if active != 3 || maximum != 3 {
		t.Fatalf("active=%d maximum=%d", active, maximum)
	}
	close(release)
	job = waitJobStatus(t, manager, job.ID, "COMPLETED")
	if job.TotalRows != 7 || job.TotalAccounts != 5 || job.Processed != 5 {
		t.Fatalf("job=%+v", job)
	}
	active, maximum, _, calls := fake.snapshot()
	if active != 0 || maximum > 3 || len(calls) != 5 || calls["A"] != 1 {
		t.Fatalf("active=%d max=%d calls=%v", active, maximum, calls)
	}
	file, _, err := manager.OpenOutput(context.Background(), job.ID, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	archive, err := zip.OpenReader(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, part := range archive.File {
		if part.Name == "xl/worksheets/sheet1.xml" {
			data, err := readPart(part)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), `<t>1234.57</t>`) != 7 || strings.Count(string(data), `<t>11.234567890123</t>`) != 7 {
				t.Fatalf("financial mapping missing from duplicate rows: %s", data)
			}
			if !strings.Contains(string(data), `<t> A </t>`) {
				t.Fatal("account cell whitespace changed")
			}
		}
	}
}

func TestMoreThanTenThousandUniqueAccountsCanQueue(t *testing.T) {
	store := newMemoryStore()
	blocked := make(chan struct{})
	fake := &controlledPositions{started: make(chan string, 32), release: blocked}
	manager, err := NewManager(context.Background(), store, fake, Config{Concurrency: 4, MaxUploadBytes: 4 << 20, StorageDir: t.TempDir()}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	baselineGoroutines := runtime.NumGoroutine()
	accounts := make([]string, 10001)
	for i := range accounts {
		accounts[i] = fmt.Sprintf("A%05d", i)
	}
	file, err := os.Open(accountsWorkbook(t, accounts))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	date, _ := loan.ParseDate("2026-08-31", time.UTC)
	identity := audit.Identity{UserID: 7, Username: "test"}
	job, err := manager.Submit(context.Background(), audit.Attribution{Actor: &identity, Effective: &identity}, "input.xlsx", date, file)
	if err != nil || job.TotalAccounts != 10001 || job.TotalRows != 10001 {
		t.Fatalf("large job=%+v err=%v", job, err)
	}
	awaitStarted(t, fake.started, 4)
	if got := runtime.NumGoroutine(); got > baselineGoroutines+64 {
		t.Fatalf("unbounded account goroutines: before=%d after=%d", baselineGoroutines, got)
	}
	if err := manager.Cancel(context.Background(), job.ID, 7, false); err != nil {
		t.Fatal(err)
	}
}

func TestCompressedUploadLimitRejectsBeforeQueue(t *testing.T) {
	store := newMemoryStore()
	directory := t.TempDir()
	manager, err := NewManager(context.Background(), store, &controlledPositions{}, Config{Concurrency: 1, MaxUploadBytes: 1, StorageDir: directory}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	file, err := os.Open(fixture(t, 1, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	date, _ := loan.ParseDate("2026-08-31", time.UTC)
	identity := audit.Identity{UserID: 7, Username: "test"}
	_, err = manager.Submit(context.Background(), audit.Attribution{Actor: &identity, Effective: &identity}, "input.xlsx", date, file)
	var validation ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("upload limit error=%v", err)
	}
	jobs, _ := store.History(context.Background(), 7, false)
	if len(jobs) != 0 {
		t.Fatalf("queued oversized upload: %+v", jobs)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary upload remained: entries=%v err=%v", entries, err)
	}
}

func TestStorageRejectsPublicAndTraversingPaths(t *testing.T) {
	_, err := NewManager(context.Background(), newMemoryStore(), &controlledPositions{}, Config{Concurrency: 1, MaxUploadBytes: 1 << 20, StorageDir: filepath.Join("..", "..", "web", "static", "slik")}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "web/static") {
		t.Fatalf("public storage accepted: %v", err)
	}
	manager := &Manager{config: Config{StorageDir: t.TempDir()}}
	if _, err := manager.safePath("../evil.input.xlsx", "../evil", ".input.xlsx"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestCompletionAuditKeepsSubmittingActor(t *testing.T) {
	store := newMemoryStore()
	events := make(chan audit.Event, 1)
	manager, err := NewManager(context.Background(), store, &controlledPositions{}, Config{Concurrency: 2, MaxUploadBytes: 1 << 20, StorageDir: t.TempDir()}, func(_ context.Context, event audit.Event) error { events <- event; return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	file, err := os.Open(accountsWorkbook(t, []string{"A"}))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	date, _ := loan.ParseDate("2026-08-31", time.UTC)
	actor := audit.Identity{UserID: 8, Username: "admin"}
	effective := audit.Identity{UserID: 7, Username: "user"}
	job, err := manager.Submit(context.Background(), audit.Attribution{Actor: &actor, Effective: &effective}, "input.xlsx", date, file)
	if err != nil {
		t.Fatal(err)
	}
	waitJobStatus(t, manager, job.ID, "COMPLETED")
	select {
	case event := <-events:
		if event.Action != audit.ActionReportingGenerate || event.Attribution.Actor.UserID != 8 || event.Attribution.Effective.UserID != 7 {
			t.Fatalf("audit=%+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("audit event missing")
	}
}

func TestGlobalOneJobFIFO(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	fake := &controlledPositions{started: make(chan string, 10), release: release}
	manager := testManager(t, store, fake, 4, t.TempDir())
	testManager(t, store, fake, 4, manager.config.StorageDir)
	first := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A"}))
	awaitStarted(t, fake.started, 1)
	second := submitWorkbook(t, manager, accountsWorkbook(t, []string{"B"}))
	job, _ := manager.Get(context.Background(), second.ID, 7, false)
	if job.Status != "QUEUED" {
		t.Fatalf("second status %s", job.Status)
	}
	_, _, _, calls := fake.snapshot()
	if calls["B"] != 0 {
		t.Fatalf("second job started: %v", calls)
	}
	close(release)
	waitJobStatus(t, manager, first.ID, "COMPLETED")
	waitJobStatus(t, manager, second.ID, "COMPLETED")
	_, _, _, calls = fake.snapshot()
	if calls["B"] != 1 {
		t.Fatalf("second job calls=%v", calls)
	}
}

func TestFirstFailureStopsSchedulingAndPublishesNoOutput(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	failNow := make(chan struct{})
	fake := &controlledPositions{started: make(chan string, 20), release: release, failAccount: "B", failRelease: failNow, lateErrorAccount: "C"}
	manager := testManager(t, store, fake, 4, t.TempDir())
	job := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A", "B", "C", "D", "E", "F", "G"}))
	started := awaitStarted(t, fake.started, 4)
	sort.Strings(started)
	if strings.Join(started, ",") != "A,B,C,D" {
		t.Fatalf("in flight=%v", started)
	}
	close(failNow)
	job = waitJobStatus(t, manager, job.ID, "FAILED")
	if job.FailedAccount != "B" || job.FailureReason != "account not found" || job.OutputFile != "" {
		t.Fatalf("job=%+v", job)
	}
	active, maximum, canceled, calls := fake.snapshot()
	if active != 0 || maximum > 4 || canceled == 0 || calls["E"] != 0 {
		t.Fatalf("active=%d max=%d canceled=%d calls=%v", active, maximum, canceled, calls)
	}
	if _, _, err := manager.OpenOutput(context.Background(), job.ID, 7, false); !errors.Is(err, ErrNotReady) {
		t.Fatalf("download=%v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.config.StorageDir, job.ID+".output.xlsx")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial output exists: %v", err)
	}
}

func TestSafeFailurePrefersHistoricalEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"account absent", loan.ErrNotFound, "account not found"},
		{"historical evidence absent", loan.ErrHistoricalEvidence, "historical evidence unavailable"},
		{"joined historical absence", errors.Join(loan.ErrNotFound, loan.ErrHistoricalEvidence), "historical evidence unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := safeFailure(test.err); got != test.want {
				t.Fatalf("safeFailure(%v)=%q want=%q", test.err, got, test.want)
			}
		})
	}
}

func TestSLIKClosedLoanAndReversalResults(t *testing.T) {
	readSheet := func(path string) []byte {
		archive, err := zip.OpenReader(path)
		if err != nil {
			t.Fatal(err)
		}
		defer archive.Close()
		for _, part := range archive.File {
			if part.Name == "xl/worksheets/sheet1.xml" {
				data, err := readPart(part)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
		}
		t.Fatal("worksheet missing")
		return nil
	}

	t.Run("closed loan keeps Fincloud rate and unrelated cells", func(t *testing.T) {
		store := newMemoryStore()
		positions := positionFunc(func(_ context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
			closeDate, _ := loan.ParseDate("2026-08-20", time.UTC)
			return loan.ResolvedPosition{
				Loan:     loan.ContractData{PrimaryAccount: account, CloseDate: closeDate, FlatRatePercent: loan.MustMoney("11.76")},
				Position: loan.LoanPosition{AsOf: asOf, AccountNumber: account, Source: loan.SourceClosed},
			}, nil
		})
		manager := testManager(t, store, positions, 1, t.TempDir())
		input := accountsWorkbook(t, []string{"A"})
		job := submitWorkbook(t, manager, input)
		job = waitJobStatus(t, manager, job.ID, "COMPLETED")
		store.mu.Lock()
		value := store.accounts[job.ID]["A"]
		store.mu.Unlock()
		if value.Balance != "0.00" || value.Rate != "11.76" {
			t.Fatalf("stored values=%+v", value)
		}
		file, _, err := manager.OpenOutput(context.Background(), job.ID, 7, false)
		if err != nil {
			t.Fatal(err)
		}
		output := file.Name()
		file.Close()
		before, after := readSheet(input), readSheet(output)
		if !bytes.Equal(maskTargets(t, before), maskTargets(t, after)) ||
			!bytes.Contains(after, []byte(`<t>0.00</t>`)) || !bytes.Contains(after, []byte(`<t>11.76</t>`)) {
			t.Fatalf("closed workbook changed outside balance/rate or missed values: %s", after)
		}
	})
	t.Run("Fincloud-native historical position keeps DWH balance and Fincloud rate", func(t *testing.T) {
		store := newMemoryStore()
		positions := positionFunc(func(_ context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
			return loan.ResolvedPosition{
				Loan:     loan.ContractData{PrimaryAccount: account, FlatRatePercent: loan.MustMoney("12.50")},
				Position: loan.LoanPosition{AsOf: asOf, AccountNumber: account, PrincipalOutstanding: loan.MustMoney("76543210"), Source: loan.SourceDWH},
			}, nil
		})
		manager := testManager(t, store, positions, 1, t.TempDir())
		input := accountsWorkbook(t, []string{"A"})
		job := submitWorkbook(t, manager, input)
		job = waitJobStatus(t, manager, job.ID, "COMPLETED")
		store.mu.Lock()
		value := store.accounts[job.ID]["A"]
		store.mu.Unlock()
		if value.Balance != "76543210.00" || value.Rate != "12.5" {
			t.Fatalf("stored values=%+v", value)
		}
		file, _, err := manager.OpenOutput(context.Background(), job.ID, 7, false)
		if err != nil {
			t.Fatal(err)
		}
		output := file.Name()
		file.Close()
		before, after := readSheet(input), readSheet(output)
		if !bytes.Equal(maskTargets(t, before), maskTargets(t, after)) ||
			!bytes.Contains(after, []byte(`<t>76543210.00</t>`)) || !bytes.Contains(after, []byte(`<t>12.5</t>`)) {
			t.Fatalf("native workbook changed outside balance/rate or missed values: %s", after)
		}
	})

	date, _ := loan.ParseDate("2026-08-31", time.UTC)
	positive := loan.Repayment{Date: date, PrincipalComponent: loan.MustMoney("300000"), TotalPayment: loan.MustMoney("300000"), SourceOrder: 0}
	negative := loan.Repayment{Date: date, PrincipalComponent: loan.MustMoney("-300000"), TotalPayment: loan.MustMoney("-300000"), SourceOrder: 1}
	t.Run("exact reversal completes", func(t *testing.T) {
		store := newMemoryStore()
		manager := testManager(t, store, syntheticReversalPositions([]loan.Repayment{positive, negative}), 1, t.TempDir())
		job := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A"}))
		job = waitJobStatus(t, manager, job.ID, "COMPLETED")
		store.mu.Lock()
		value := store.accounts[job.ID]["A"]
		store.mu.Unlock()
		if value.Balance != "100.00" || value.Rate != "18" || job.OutputFile == "" {
			t.Fatalf("job=%+v values=%+v", job, value)
		}
	})
	t.Run("mixed-sign positive repayment completes", func(t *testing.T) {
		store := newMemoryStore()
		mixed := loan.Repayment{Date: date, PrincipalComponent: loan.MustMoney("336501"), InterestComponent: loan.MustMoney("-4001"), TotalPayment: loan.MustMoney("332500")}
		manager := testManager(t, store, syntheticReversalPositions([]loan.Repayment{mixed}), 1, t.TempDir())
		job := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A"}))
		job = waitJobStatus(t, manager, job.ID, "COMPLETED")
		store.mu.Lock()
		value := store.accounts[job.ID]["A"]
		store.mu.Unlock()
		if value.Balance != "0.00" || value.Rate != "18" {
			t.Fatalf("job=%+v values=%+v", job, value)
		}
	})
	t.Run("zero-net adjustment fails safely", func(t *testing.T) {
		store := newMemoryStore()
		adjustment := loan.Repayment{Date: date, PrincipalComponent: loan.MustMoney("100"), InterestComponent: loan.MustMoney("-100")}
		manager := testManager(t, store, syntheticReversalPositions([]loan.Repayment{adjustment}), 1, t.TempDir())
		job := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A"}))
		job = waitJobStatus(t, manager, job.ID, "FAILED")
		if job.FailedAccount != "A" || job.FailureReason != "unsupported zero-net repayment adjustment" || job.OutputFile != "" {
			t.Fatalf("job=%+v", job)
		}
	})
	t.Run("unsupported reversal fails fast", func(t *testing.T) {
		store := newMemoryStore()
		var callsMu sync.Mutex
		calls := make(map[string]int)
		calculate := syntheticReversalPositions([]loan.Repayment{negative})
		positions := positionFunc(func(ctx context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
			callsMu.Lock()
			calls[account]++
			callsMu.Unlock()
			return calculate(ctx, account, asOf)
		})
		manager := testManager(t, store, positions, 1, t.TempDir())
		job := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A", "B"}))
		job = waitJobStatus(t, manager, job.ID, "FAILED")
		callsMu.Lock()
		secondCalls := calls["B"]
		callsMu.Unlock()
		if job.FailedAccount != "A" || job.FailureReason != "unsupported repayment reversal" || job.OutputFile != "" || secondCalls != 0 {
			t.Fatalf("job=%+v second account calls=%d", job, secondCalls)
		}
	})
}

func TestCancelStopsInFlight(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	fake := &controlledPositions{started: make(chan string, 20), release: release}
	manager := testManager(t, store, fake, 4, t.TempDir())
	job := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A", "B", "C", "D", "E"}))
	awaitStarted(t, fake.started, 4)
	if err := manager.Cancel(context.Background(), job.ID, 7, false); err != nil {
		t.Fatal(err)
	}
	waitJobStatus(t, manager, job.ID, "CANCELED")
	manager.Close()
	active, _, canceled, calls := fake.snapshot()
	if active != 0 || canceled != 4 || calls["E"] != 0 {
		t.Fatalf("active=%d canceled=%d calls=%v", active, canceled, calls)
	}
}

func TestCancelQueuedJobNeverStarts(t *testing.T) {
	store := newMemoryStore()
	release := make(chan struct{})
	fake := &controlledPositions{started: make(chan string, 10), release: release}
	manager := testManager(t, store, fake, 1, t.TempDir())
	first := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A"}))
	awaitStarted(t, fake.started, 1)
	second := submitWorkbook(t, manager, accountsWorkbook(t, []string{"B"}))
	if err := manager.Cancel(context.Background(), second.ID, 7, false); err != nil {
		t.Fatal(err)
	}
	if job := waitJobStatus(t, manager, second.ID, "CANCELED"); job.OutputFile != "" {
		t.Fatalf("canceled output=%s", job.OutputFile)
	}
	close(release)
	waitJobStatus(t, manager, first.ID, "COMPLETED")
	_, _, _, calls := fake.snapshot()
	if calls["B"] != 0 {
		t.Fatalf("queued job started: %v", calls)
	}
}

type delayedClaimStore struct {
	*memoryStore
	claimed chan struct{}
	release chan struct{}
}

func (store *delayedClaimStore) ClaimNext(ctx context.Context) (Job, bool, error) {
	job, found, err := store.memoryStore.ClaimNext(ctx)
	if found {
		close(store.claimed)
		<-store.release
	}
	return job, found, err
}

func TestCancelBetweenClaimAndRunStartsNoAccounts(t *testing.T) {
	store := &delayedClaimStore{memoryStore: newMemoryStore(), claimed: make(chan struct{}), release: make(chan struct{})}
	fake := &controlledPositions{}
	manager, err := NewManager(context.Background(), store, fake, Config{Concurrency: 4, MaxUploadBytes: 1 << 20, StorageDir: t.TempDir()}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	job := submitWorkbook(t, manager, accountsWorkbook(t, []string{"A", "B"}))
	select {
	case <-store.claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("job was not claimed")
	}
	if err := manager.Cancel(context.Background(), job.ID, 7, false); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	waitJobStatus(t, manager, job.ID, "CANCELED")
	manager.Close()
	_, _, _, calls := fake.snapshot()
	if len(calls) != 0 {
		t.Fatalf("started canceled job: %v", calls)
	}
}

func TestRestartResumesOnlyRemainingAccounts(t *testing.T) {
	store := newMemoryStore()
	directory := t.TempDir()
	release := make(chan struct{})
	firstFake := &controlledPositions{started: make(chan string, 10), release: release, blockAccount: "C"}
	first := testManager(t, store, firstFake, 1, directory)
	job := submitWorkbook(t, first, accountsWorkbook(t, []string{"A", "B", "C", "D"}))
	started := awaitStarted(t, firstFake.started, 3)
	if strings.Join(started, ",") != "A,B,C" {
		t.Fatalf("started=%v", started)
	}
	first.Close()
	checkpoint, _ := store.Get(context.Background(), job.ID, 7, false)
	if checkpoint.Status != "PROCESSING" || checkpoint.Processed != 2 {
		t.Fatalf("checkpoint=%+v", checkpoint)
	}
	secondFake := &controlledPositions{started: make(chan string, 10)}
	second := testManager(t, store, secondFake, 2, directory)
	completed := waitJobStatus(t, second, job.ID, "COMPLETED")
	if completed.Processed != 4 {
		t.Fatalf("processed=%d", completed.Processed)
	}
	_, _, _, calls := secondFake.snapshot()
	if calls["A"] != 0 || calls["B"] != 0 || calls["C"] != 1 || calls["D"] != 1 {
		t.Fatalf("recomputed=%v", calls)
	}
}

func TestLostGlobalLockStopsWorkersAndResumes(t *testing.T) {
	store := newMemoryStore()
	store.lockLost = make(chan struct{})
	directory := t.TempDir()
	blocked := make(chan struct{})
	firstFake := &controlledPositions{started: make(chan string, 10), release: blocked, blockAccount: "B"}
	first := testManager(t, store, firstFake, 1, directory)
	job := submitWorkbook(t, first, accountsWorkbook(t, []string{"A", "B"}))
	awaitStarted(t, firstFake.started, 2)
	close(store.lockLost)
	deadline := time.After(5 * time.Second)
	for {
		active, _, canceled, _ := firstFake.snapshot()
		if active == 0 && canceled == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("workers did not stop after lock loss")
		case <-time.After(time.Millisecond):
		}
	}
	first.Close()
	interrupted, _ := store.Get(context.Background(), job.ID, 7, false)
	if interrupted.Status != "PROCESSING" || interrupted.Processed != 1 {
		t.Fatalf("interrupted=%+v", interrupted)
	}
	store.mu.Lock()
	store.lockLost = nil
	store.mu.Unlock()
	secondFake := &controlledPositions{}
	second := testManager(t, store, secondFake, 1, directory)
	waitJobStatus(t, second, job.ID, "COMPLETED")
	_, _, _, calls := secondFake.snapshot()
	if calls["A"] != 0 || calls["B"] != 1 {
		t.Fatalf("recomputed=%v", calls)
	}
}

func TestTerminalJobsDoNotResumeAndRetention(t *testing.T) {
	store := newMemoryStore()
	directory := t.TempDir()
	now := time.Now()
	for index, status := range []string{"FAILED", "COMPLETED", "CANCELED", "QUEUED"} {
		id := fmt.Sprintf("%032x", index+1)
		job := Job{ID: id, OwnerID: 7, Status: status, InputFile: id + ".input.xlsx", OutputFile: id + ".output.xlsx", CreatedAt: now.Add(-9 * 24 * time.Hour)}
		if status != "QUEUED" {
			expired := now.Add(-time.Hour)
			job.ExpiresAt = &expired
		}
		store.jobs[id] = job
		store.accounts[id] = map[string]Values{}
		if err := os.WriteFile(filepath.Join(directory, job.InputFile), []byte("input"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, job.OutputFile), []byte("output"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &Manager{store: store, config: Config{StorageDir: directory}, root: context.Background(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	manager.cleanup()
	for id, job := range store.jobs {
		if job.Status == "QUEUED" {
			if _, err := os.Stat(filepath.Join(directory, job.InputFile)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if job.InputFile != "" || job.OutputFile != "" {
			t.Fatalf("retention metadata %s: %+v", id, job)
		}
		if _, err := os.Stat(filepath.Join(directory, id+".input.xlsx")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retention file %s: %v", id, err)
		}
	}
}

func TestRecoveryLeavesTerminalJobsUntouched(t *testing.T) {
	store := newMemoryStore()
	for index, status := range []string{"FAILED", "COMPLETED", "CANCELED"} {
		id := fmt.Sprintf("%032x", index+1)
		store.jobs[id] = Job{ID: id, OwnerID: 7, Status: status}
		store.accounts[id] = map[string]Values{"A": {}}
	}
	fake := &controlledPositions{started: make(chan string, 3)}
	manager := testManager(t, store, fake, 4, t.TempDir())
	select {
	case <-store.recovered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not run")
	}
	manager.Close()
	for id, job := range store.jobs {
		if job.Status != "FAILED" && job.Status != "COMPLETED" && job.Status != "CANCELED" {
			t.Fatalf("resumed terminal job %s: %+v", id, job)
		}
	}
	_, _, _, calls := fake.snapshot()
	if len(calls) != 0 {
		t.Fatalf("terminal calls=%v", calls)
	}
}

func TestRetentionKeepsRecentTerminalAndOldQueued(t *testing.T) {
	store := newMemoryStore()
	directory := t.TempDir()
	now := time.Now()
	for index, item := range []struct {
		status string
		expiry *time.Time
	}{
		{"COMPLETED", pointerTime(now.Add(time.Hour))},
		{"QUEUED", nil},
	} {
		id := fmt.Sprintf("%032x", index+1)
		job := Job{ID: id, OwnerID: 7, Status: item.status, InputFile: id + ".input.xlsx", CreatedAt: now.Add(-10 * 24 * time.Hour), ExpiresAt: item.expiry}
		store.jobs[id] = job
		if err := os.WriteFile(filepath.Join(directory, job.InputFile), []byte("input"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	manager := &Manager{store: store, config: Config{StorageDir: directory}, root: context.Background(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	manager.cleanup()
	for _, job := range store.jobs {
		if _, err := os.Stat(filepath.Join(directory, job.InputFile)); err != nil {
			t.Fatalf("file removed early: %v", err)
		}
	}
}

func TestExpiredOutputCannotDownload(t *testing.T) {
	store := newMemoryStore()
	directory := t.TempDir()
	id := "0123456789abcdef0123456789abcdef"
	expired := time.Now().Add(-time.Second)
	store.jobs[id] = Job{ID: id, OwnerID: 7, Status: "COMPLETED", OutputFile: id + ".output.xlsx", ExpiresAt: &expired}
	if err := os.WriteFile(filepath.Join(directory, id+".output.xlsx"), []byte("output"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{store: store, config: Config{StorageDir: directory}}
	if _, _, err := manager.OpenOutput(context.Background(), id, 7, false); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired download=%v", err)
	}
}
func pointerTime(value time.Time) *time.Time { return &value }

func BenchmarkSLIKWorkers(b *testing.B) {
	for _, concurrency := range []int{1, 16} {
		b.Run(fmt.Sprintf("concurrency_%d", concurrency), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				store := newMemoryStore()
				directory := b.TempDir()
				fake := &sleepPositions{delay: 2 * time.Millisecond}
				manager, err := NewManager(context.Background(), store, fake, Config{Concurrency: concurrency, MaxUploadBytes: 1 << 20, StorageDir: directory}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					b.Fatal(err)
				}
				var accounts []string
				for n := 0; n < 64; n++ {
					accounts = append(accounts, fmt.Sprintf("%04d", n))
				}
				input := accountsWorkbook(b, accounts)
				file, err := os.Open(input)
				if err != nil {
					b.Fatal(err)
				}
				date, _ := loan.ParseDate("2026-08-31", time.UTC)
				identity := audit.Identity{UserID: 7, Username: "test"}
				job, err := manager.Submit(context.Background(), audit.Attribution{Actor: &identity, Effective: &identity}, "input.xlsx", date, file)
				file.Close()
				if err != nil {
					b.Fatal(err)
				}
				deadline := time.After(5 * time.Second)
				for {
					result, err := manager.Get(context.Background(), job.ID, 7, false)
					if err != nil {
						b.Fatal(err)
					}
					if result.Status == "COMPLETED" {
						break
					}
					select {
					case <-deadline:
						b.Fatal("benchmark job timeout")
					case <-time.After(time.Millisecond):
					}
				}
				manager.Close()
			}
		})
	}
}

type sleepPositions struct{ delay time.Duration }

func (fake *sleepPositions) GetLoanPosition(ctx context.Context, account string, asOf loan.Date) (loan.ResolvedPosition, error) {
	select {
	case <-time.After(fake.delay):
	case <-ctx.Done():
		return loan.ResolvedPosition{}, ctx.Err()
	}
	return loan.ResolvedPosition{Loan: loan.ContractData{PrimaryAccount: account, FlatRatePercent: loan.MustMoney("12")}, Position: loan.LoanPosition{AsOf: asOf, PrincipalOutstanding: loan.MustMoney("10")}}, nil
}
