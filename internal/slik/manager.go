package slik

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ibldzn/trs/internal/audit"
	"github.com/ibldzn/trs/internal/loan"
)

type PositionService interface {
	GetLoanPosition(context.Context, string, loan.Date) (loan.ResolvedPosition, error)
}

type Store interface {
	Create(context.Context, Job, []string) error
	Get(context.Context, string, uint64, bool) (Job, error)
	History(context.Context, uint64, bool) ([]Job, error)
	Recover(context.Context) error
	ClaimNext(context.Context) (Job, bool, error)
	Pending(context.Context, string, string, int) ([]string, error)
	Save(context.Context, string, string, string, Values) error
	Results(context.Context, string) (map[string]Values, error)
	Terminal(context.Context, string, string, string, string, string) error
	Cancel(context.Context, string, uint64, bool) error
	IsCanceled(context.Context, string) (bool, error)
	Expired(context.Context) ([]Job, error)
	ClearFiles(context.Context, string) error
	AcquireLock(context.Context) (func(), <-chan struct{}, bool, error)
	CheckComplete(context.Context, string) error
}

type Config struct {
	Concurrency    int
	MaxUploadBytes int64
	StorageDir     string
}

type ValidationError struct{ Reason string }

func (err ValidationError) Error() string { return err.Reason }

type Manager struct {
	store        Store
	positions    PositionService
	config       Config
	logger       *slog.Logger
	appendAudit  func(context.Context, audit.Event) error
	root         context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	wake         chan struct{}
	mu           sync.Mutex
	activeID     string
	activeCancel context.CancelFunc
}

func NewManager(parent context.Context, store Store, positions PositionService, config Config, appendAudit func(context.Context, audit.Event) error, logger *slog.Logger) (*Manager, error) {
	if parent == nil || store == nil || positions == nil || config.Concurrency < 1 || config.Concurrency > 32 || config.MaxUploadBytes < 1 || config.MaxUploadBytes > 256<<20 || config.StorageDir == "" {
		return nil, errors.New("invalid SLIK manager configuration")
	}
	var err error
	config.StorageDir, err = filepath.Abs(config.StorageDir)
	if err != nil {
		return nil, err
	}
	publicDir, err := filepath.Abs(filepath.Join("web", "static"))
	if err != nil {
		return nil, err
	}
	storagePath := filepath.ToSlash(config.StorageDir)
	if underDir(publicDir, config.StorageDir) || strings.Contains(storagePath, "/web/static/") || strings.HasSuffix(storagePath, "/web/static") {
		return nil, errors.New("SLIK storage cannot be under web/static")
	}
	if err := os.MkdirAll(config.StorageDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(config.StorageDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("SLIK storage must be a directory, not a symlink")
	}
	resolvedStorage, err := filepath.EvalSymlinks(config.StorageDir)
	if err != nil {
		return nil, err
	}
	resolvedPublic, err := filepath.EvalSymlinks(publicDir)
	if err == nil && underDir(resolvedPublic, resolvedStorage) {
		return nil, errors.New("SLIK storage cannot be under web/static")
	}
	if err := os.Chmod(config.StorageDir, 0700); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	root, cancel := context.WithCancel(parent)
	manager := &Manager{store: store, positions: positions, config: config, appendAudit: appendAudit, logger: logger, root: root, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1)}
	go manager.loop()
	return manager, nil
}

func underDir(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func (manager *Manager) Close() { manager.cancel(); <-manager.done }

func (manager *Manager) Submit(ctx context.Context, attribution audit.Attribution, originalName string, asOf loan.Date, source io.Reader) (Job, error) {
	if attribution.Actor == nil || attribution.Effective == nil || attribution.Actor.UserID == 0 || attribution.Effective.UserID == 0 || attribution.Actor.Username == "" || attribution.Effective.Username == "" || asOf.IsZero() || source == nil || originalName == "" || filepath.Base(originalName) != originalName || strings.Contains(originalName, "\\") || len(originalName) > 255 {
		return Job{}, ValidationError{"invalid SLIK submission"}
	}
	if !strings.EqualFold(filepath.Ext(originalName), ".xlsx") {
		return Job{}, ValidationError{"only .xlsx files are accepted"}
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return Job{}, err
	}
	id := hex.EncodeToString(token[:])
	inputName := id + ".input.xlsx"
	inputPath := filepath.Join(manager.config.StorageDir, inputName)
	file, err := os.OpenFile(inputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Job{}, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(inputPath)
		}
	}()
	count, copyErr := io.Copy(file, io.LimitReader(source, manager.config.MaxUploadBytes+1))
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		err = copyErr
		return Job{}, err
	}
	if closeErr != nil {
		err = closeErr
		return Job{}, err
	}
	if count == 0 || count > manager.config.MaxUploadBytes {
		err = ValidationError{"XLSX exceeds compressed upload limit"}
		return Job{}, err
	}
	workbook, inspectErr := Inspect(inputPath, originalName)
	if inspectErr != nil {
		err = ValidationError{inspectErr.Error()}
		return Job{}, err
	}
	seen := make(map[string]struct{})
	accounts := make([]string, 0, len(workbook.Rows))
	for _, row := range workbook.Rows {
		if len([]rune(row.Account)) > 191 {
			err = ValidationError{"SLIK account identifier exceeds 191 characters"}
			return Job{}, err
		}
		if _, exists := seen[row.Account]; !exists {
			seen[row.Account] = struct{}{}
			accounts = append(accounts, row.Account)
		}
	}
	job := Job{ID: id, OwnerID: attribution.Effective.UserID, OwnerUsername: attribution.Effective.Username, ActorID: attribution.Actor.UserID, ActorUsername: attribution.Actor.Username, OriginalFilename: originalName, AsOf: asOf.String(), Status: "QUEUED", TotalRows: int64(len(workbook.Rows)), TotalAccounts: int64(len(accounts)), CreatedAt: time.Now().UTC(), InputFile: inputName}
	if createErr := manager.store.Create(ctx, job, accounts); createErr != nil {
		err = createErr
		return Job{}, err
	}
	select {
	case manager.wake <- struct{}{}:
	default:
	}
	manager.logger.Info("SLIK job queued", "job_id", id, "total_rows", job.TotalRows, "unique_accounts", job.TotalAccounts, "concurrency", manager.config.Concurrency)
	return job, nil
}

func (manager *Manager) Get(ctx context.Context, id string, owner uint64, admin bool) (Job, error) {
	readContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return manager.store.Get(readContext, id, owner, admin)
}
func (manager *Manager) History(ctx context.Context, owner uint64, admin bool) ([]Job, error) {
	readContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return manager.store.History(readContext, owner, admin)
}

func (manager *Manager) Cancel(ctx context.Context, id string, owner uint64, admin bool) error {
	writeContext, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := manager.store.Cancel(writeContext, id, owner, admin); err != nil {
		return err
	}
	manager.mu.Lock()
	if manager.activeID == id && manager.activeCancel != nil {
		manager.activeCancel()
	}
	manager.mu.Unlock()
	return nil
}

func (manager *Manager) OpenOutput(ctx context.Context, id string, owner uint64, admin bool) (*os.File, Job, error) {
	readContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	job, err := manager.store.Get(readContext, id, owner, admin)
	if err != nil {
		return nil, Job{}, err
	}
	if job.Status != "COMPLETED" {
		return nil, job, ErrNotReady
	}
	if !job.Available() {
		return nil, job, ErrExpired
	}
	filename, err := manager.safePath(job.OutputFile, job.ID, ".output.xlsx")
	if err != nil {
		return nil, job, ErrExpired
	}
	file, err := os.Open(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, job, ErrExpired
	}
	return file, job, err
}

func (job Job) Available() bool {
	return job.Status == "COMPLETED" && job.OutputFile != "" && job.ExpiresAt != nil && time.Now().Before(*job.ExpiresAt)
}

func (manager *Manager) safePath(name, id, suffix string) (string, error) {
	if !validJobID(id) || name != id+suffix || filepath.Base(name) != name {
		return "", errors.New("unsafe SLIK storage reference")
	}
	return filepath.Join(manager.config.StorageDir, name), nil
}

func validJobID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (manager *Manager) loop() {
	defer close(manager.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	cleanupTicker := time.NewTicker(time.Hour)
	defer cleanupTicker.Stop()
	for {
		if manager.root.Err() != nil {
			return
		}
		release, lockLost, acquired, err := manager.store.AcquireLock(manager.root)
		if err != nil {
			manager.logger.Error("SLIK lock failed", "error", err)
		}
		if acquired {
			pollContext, stopPoll := context.WithTimeout(manager.root, 15*time.Second)
			var job Job
			var found bool
			if err := manager.store.Recover(pollContext); err != nil {
				manager.logger.Error("SLIK recovery failed", "error", err)
			} else {
				job, found, err = manager.store.ClaimNext(pollContext)
				if err != nil {
					manager.logger.Error("SLIK claim failed", "error", err)
				}
			}
			stopPoll()
			if found {
				manager.run(job, lockLost)
			}
			release()
		}
		select {
		case <-manager.root.Done():
			return
		case <-manager.wake:
		case <-ticker.C:
		case <-cleanupTicker.C:
			manager.cleanup()
		}
	}
}

func (manager *Manager) run(job Job, lockLost <-chan struct{}) {
	ctx, cancel := context.WithCancel(manager.root)
	manager.mu.Lock()
	manager.activeID = job.ID
	manager.activeCancel = cancel
	manager.mu.Unlock()
	defer func() {
		cancel()
		manager.mu.Lock()
		manager.activeID = ""
		manager.activeCancel = nil
		manager.mu.Unlock()
	}()
	started := time.Now()
	manager.logger.Info("SLIK job started", "job_id", job.ID, "total_rows", job.TotalRows, "unique_accounts", job.TotalAccounts, "concurrency", manager.config.Concurrency)
	if !validJobID(job.ID) {
		manager.finish(job, "FAILED", "", "invalid SLIK job identifier", "", started)
		return
	}
	outputName := job.ID + ".output.xlsx"
	output := filepath.Join(manager.config.StorageDir, outputName)
	if err := os.Remove(output); err != nil && !errors.Is(err, os.ErrNotExist) {
		manager.finish(job, "FAILED", "", "SLIK output storage unavailable", "", started)
		return
	}
	check, stop := context.WithTimeout(ctx, 5*time.Second)
	canceled, err := manager.store.IsCanceled(check, job.ID)
	stop()
	if err != nil {
		manager.logger.Error("SLIK cancellation check failed", "job_id", job.ID, "error", err)
		return
	}
	if canceled {
		manager.logDone(job, "CANCELED", started)
		return
	}
	select {
	case <-lockLost:
		return
	default:
	}
	stopWatcher := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatcher:
				return
			case <-lockLost:
				cancel()
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(manager.root, 5*time.Second)
				canceled, err := manager.store.IsCanceled(check, job.ID)
				stop()
				if err == nil && canceled {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(stopWatcher); <-watcherDone }()
	work := make(chan string)
	var wait sync.WaitGroup
	var first sync.Once
	var failedAccount, failureReason string
	fail := func(account string, err error) {
		if ctx.Err() != nil {
			return
		}
		first.Do(func() {
			failedAccount = account
			failureReason = safeFailure(err)
			cancel()
			manager.logger.Error("SLIK account failed", "job_id", job.ID, "reason", failureReason, "error", err)
		})
	}
	date, err := loan.ParseDate(job.AsOf, time.UTC)
	if err != nil {
		fail("", err)
	}
	for range manager.config.Concurrency {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for account := range work {
				if ctx.Err() != nil {
					return
				}
				accountContext, stopAccount := context.WithTimeout(ctx, 2*time.Minute)
				resolved, err := manager.positions.GetLoanPosition(accountContext, account, date)
				stopAccount()
				if err != nil {
					fail(account, err)
					return
				}
				rate, err := resolved.Loan.FlatRatePercent.ExactDecimal()
				if err != nil {
					fail(account, err)
					return
				}
				values := Values{Balance: resolved.Position.PrincipalOutstanding.Format(2), Rate: rate}
				saveContext, stopSave := context.WithTimeout(ctx, 15*time.Second)
				err = manager.store.Save(saveContext, job.ID, account, resolved.Loan.PrimaryAccount, values)
				stopSave()
				if err != nil {
					fail(account, err)
					return
				}
			}
		}()
	}
	var after string
produce:
	for ctx.Err() == nil {
		pageContext, stopPage := context.WithTimeout(ctx, 15*time.Second)
		accounts, err := manager.store.Pending(pageContext, job.ID, after, 128)
		stopPage()
		if err != nil {
			fail("", err)
			break
		}
		if len(accounts) == 0 {
			break
		}
		for _, account := range accounts {
			if ctx.Err() != nil {
				break produce
			}
			select {
			case work <- account:
				after = account
			case <-ctx.Done():
				break produce
			}
		}
	}
	close(work)
	wait.Wait()
	select {
	case <-lockLost:
		return
	default:
	}
	if manager.root.Err() != nil {
		return
	} // Shutdown leaves PROCESSING for recovery.
	check, stop = context.WithTimeout(context.Background(), 15*time.Second)
	canceled, cancelErr := manager.store.IsCanceled(check, job.ID)
	stop()
	if cancelErr != nil {
		manager.logger.Error("SLIK cancellation check failed", "job_id", job.ID, "error", cancelErr)
		return
	}
	if canceled {
		manager.logDone(job, "CANCELED", started)
		return
	}
	if failureReason != "" {
		manager.finish(job, "FAILED", failedAccount, failureReason, "", started)
		return
	}
	if ctx.Err() != nil {
		select {
		case <-lockLost:
			return
		default:
		}
		manager.finish(job, "FAILED", "", "SLIK processing interrupted", "", started)
		return
	}
	checkContext, stopCheck := context.WithTimeout(manager.root, 15*time.Second)
	err = manager.store.CheckComplete(checkContext, job.ID)
	stopCheck()
	if err != nil {
		manager.finish(job, "FAILED", "", "SLIK checkpoints incomplete", "", started)
		return
	}
	input, _ := manager.safePath(job.InputFile, job.ID, ".input.xlsx")
	workbook, err := Inspect(input, job.OriginalFilename)
	if err != nil {
		manager.finish(job, "FAILED", "", "stored workbook unavailable", "", started)
		return
	}
	resultsContext, stopResults := context.WithTimeout(manager.root, 2*time.Minute)
	results, err := manager.store.Results(resultsContext, job.ID)
	stopResults()
	if err != nil {
		manager.finish(job, "FAILED", "", "SLIK checkpoints unavailable", "", started)
		return
	}
	if err := Patch(input, output, workbook, results); err != nil {
		_ = os.Remove(output)
		manager.finish(job, "FAILED", "", "XLSX generation failed", "", started)
		return
	}
	select {
	case <-lockLost:
		_ = os.Remove(output)
		return
	default:
	}
	check, stop = context.WithTimeout(context.Background(), 15*time.Second)
	canceled, cancelErr = manager.store.IsCanceled(check, job.ID)
	stop()
	if cancelErr != nil || canceled {
		_ = os.Remove(output)
		manager.logDone(job, "CANCELED", started)
		return
	}
	select {
	case <-lockLost:
		_ = os.Remove(output)
		return
	default:
	}
	manager.finish(job, "COMPLETED", "", "", outputName, started)
}

func (manager *Manager) finish(job Job, status, account, reason, output string, started time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := manager.store.Terminal(ctx, job.ID, status, account, reason, output); err != nil {
		if output != "" {
			_ = os.Remove(filepath.Join(manager.config.StorageDir, output))
		}
		manager.logger.Error("SLIK terminal update failed", "job_id", job.ID, "error", err)
		return
	}
	manager.logDone(job, status, started)
	if status == "COMPLETED" && manager.appendAudit != nil {
		actor := audit.Identity{UserID: job.ActorID, Username: job.ActorUsername}
		effective := audit.Identity{UserID: job.OwnerID, Username: job.OwnerUsername}
		if err := manager.appendAudit(ctx, audit.Event{Attribution: audit.Attribution{Actor: &actor, Effective: &effective}, Action: audit.ActionReportingGenerate, Metadata: audit.ReportingMetadata{AsOf: job.AsOf, RowCount: int(job.TotalRows)}, CreatedAt: time.Now().UTC()}); err != nil {
			manager.logger.Warn("SLIK audit append failed", "job_id", job.ID, "error", err)
		}
	}
}

func (manager *Manager) logDone(job Job, status string, started time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := manager.store.Get(ctx, job.ID, 0, true)
	if err != nil {
		current = job
	}
	elapsed := time.Since(started).Seconds()
	throughput := float64(current.Processed) / max(elapsed, 0.001)
	manager.logger.Info("SLIK job finished", "job_id", job.ID, "status", status, "total_rows", job.TotalRows, "unique_accounts", job.TotalAccounts, "concurrency", manager.config.Concurrency, "processed", current.Processed, "elapsed_seconds", elapsed, "accounts_per_second", throughput)
}

func (manager *Manager) cleanup() {
	ctx, cancel := context.WithTimeout(manager.root, 30*time.Second)
	defer cancel()
	jobs, err := manager.store.Expired(ctx)
	if err != nil {
		manager.logger.Error("SLIK retention lookup failed", "error", err)
		return
	}
	for _, job := range jobs {
		removed := true
		for _, item := range []struct{ name, suffix string }{{job.InputFile, ".input.xlsx"}, {job.OutputFile, ".output.xlsx"}} {
			if item.name == "" {
				continue
			}
			filename, err := manager.safePath(item.name, job.ID, item.suffix)
			if err != nil {
				manager.logger.Error("unsafe SLIK storage reference", "job_id", job.ID)
				removed = false
				continue
			}
			if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
				manager.logger.Error("SLIK retention removal failed", "job_id", job.ID, "error", err)
				removed = false
				continue
			}
		}
		if removed {
			if err := manager.store.ClearFiles(ctx, job.ID); err != nil {
				manager.logger.Error("SLIK retention metadata failed", "job_id", job.ID, "error", err)
			}
		}
	}
}

func safeFailure(err error) string {
	switch {
	case errors.Is(err, loan.ErrAmbiguousAccountResolution):
		return "ambiguous account"
	case errors.Is(err, loan.ErrFincloudUnavailable), errors.Is(err, loan.ErrFincloudSession), errors.Is(err, loan.ErrFincloudCredentials):
		return "Fincloud unavailable"
	case errors.Is(err, loan.ErrMSOUnavailable):
		return "MSO evidence unavailable"
	case errors.Is(err, loan.ErrDWHUnavailable):
		return "DWH evidence unavailable"
	case errors.Is(err, loan.ErrCurrentSnapshot):
		return "current snapshot unavailable"
	case errors.Is(err, loan.ErrNotFound):
		return "account not found"
	case errors.Is(err, loan.ErrHistoricalEvidence):
		return "historical evidence unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream timeout"
	case errors.Is(err, loan.ErrUnsupportedRepaymentReversal):
		return "unsupported repayment reversal"
	case errors.Is(err, loan.ErrUnsupportedRepaymentAdjustment):
		return "unsupported zero-net repayment adjustment"
	case errors.Is(err, loan.ErrUnsupportedCalculation):
		return "unsupported calculation"
	case errors.Is(err, loan.ErrInvalidInput):
		return "invalid account or reporting date"
	default:
		return "SLIK processing unavailable"
	}
}
