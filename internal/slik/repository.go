package slik

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

var (
	ErrNotFound = errors.New("SLIK job not found")
	ErrNotReady = errors.New("SLIK output is not ready")
	ErrExpired  = errors.New("SLIK output has expired")
)

const jobColumns = `id, owner_user_id, owner_username, actor_user_id, actor_username, original_filename, DATE_FORMAT(reporting_date, '%Y-%m-%d') AS as_of,
 status, total_rows, total_accounts, processed_accounts, created_at, started_at, finished_at,
 COALESCE(failed_account, '') AS failed_account, COALESCE(failure_reason, '') AS failure_reason,
 input_file, COALESCE(output_file, '') AS output_file, expires_at`

type Job struct {
	ID               string     `db:"id"`
	OwnerID          uint64     `db:"owner_user_id"`
	OwnerUsername    string     `db:"owner_username"`
	ActorID          uint64     `db:"actor_user_id"`
	ActorUsername    string     `db:"actor_username"`
	OriginalFilename string     `db:"original_filename"`
	AsOf             string     `db:"as_of"`
	Status           string     `db:"status"`
	TotalRows        int64      `db:"total_rows"`
	TotalAccounts    int64      `db:"total_accounts"`
	Processed        int64      `db:"processed_accounts"`
	CreatedAt        time.Time  `db:"created_at"`
	StartedAt        *time.Time `db:"started_at"`
	FinishedAt       *time.Time `db:"finished_at"`
	FailedAccount    string     `db:"failed_account"`
	FailureReason    string     `db:"failure_reason"`
	InputFile        string     `db:"input_file"`
	OutputFile       string     `db:"output_file"`
	ExpiresAt        *time.Time `db:"expires_at"`
}

type Checkpoint struct {
	Account string `db:"requested_account"`
	Balance string `db:"bakidebet"`
	Rate    string `db:"sukubungaimbalan"`
}

type Repository struct{ db *sqlx.DB }

func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (repository *Repository) Create(ctx context.Context, job Job, accounts []string) error {
	transaction, err := repository.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	_, err = transaction.ExecContext(ctx, `INSERT INTO slik_jobs
	(id, owner_user_id, owner_username, actor_user_id, actor_username, original_filename, reporting_date, status, total_rows, total_accounts, created_at, input_file)
	VALUES (?, ?, ?, ?, ?, ?, ?, 'QUEUED', ?, ?, ?, ?)`, job.ID, job.OwnerID, job.OwnerUsername, job.ActorID, job.ActorUsername, job.OriginalFilename, job.AsOf, job.TotalRows, len(accounts), job.CreatedAt, job.InputFile)
	if err != nil {
		return err
	}
	for start := 0; start < len(accounts); start += 200 {
		batch := accounts[start:min(start+200, len(accounts))]
		var query strings.Builder
		query.WriteString(`INSERT INTO slik_job_accounts (job_id, requested_account, status) VALUES `)
		args := make([]any, 0, len(batch)*2)
		for i, account := range batch {
			if i != 0 {
				query.WriteByte(',')
			}
			query.WriteString(`(?, ?, 'QUEUED')`)
			args = append(args, job.ID, account)
		}
		if _, err := transaction.ExecContext(ctx, query.String(), args...); err != nil {
			return err
		}
	}
	return transaction.Commit()
}

func (repository *Repository) Get(ctx context.Context, id string, ownerID uint64, viewAll bool) (Job, error) {
	query := `SELECT ` + jobColumns + ` FROM slik_jobs WHERE id = ?`
	args := []any{id}
	if !viewAll {
		query += ` AND owner_user_id = ?`
		args = append(args, ownerID)
	}
	var job Job
	if err := repository.db.GetContext(ctx, &job, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNotFound
		}
		return Job{}, err
	}
	return job, nil
}

func (repository *Repository) History(ctx context.Context, ownerID uint64, viewAll bool) ([]Job, error) {
	query := `SELECT ` + jobColumns + ` FROM slik_jobs`
	args := []any{}
	if !viewAll {
		query += ` WHERE owner_user_id = ?`
		args = append(args, ownerID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT 30`
	var jobs []Job
	err := repository.db.SelectContext(ctx, &jobs, query, args...)
	return jobs, err
}

func (repository *Repository) Recover(ctx context.Context) error {
	_, err := repository.db.ExecContext(ctx, `UPDATE slik_jobs SET status='QUEUED' WHERE status='PROCESSING'`)
	return err
}

func (repository *Repository) ClaimNext(ctx context.Context) (Job, bool, error) {
	for {
		var id string
		err := repository.db.GetContext(ctx, &id, `SELECT id FROM slik_jobs WHERE status='QUEUED' ORDER BY created_at, id LIMIT 1`)
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, false, nil
		}
		if err != nil {
			return Job{}, false, err
		}
		result, err := repository.db.ExecContext(ctx, `UPDATE slik_jobs SET status='PROCESSING', started_at=COALESCE(started_at, UTC_TIMESTAMP(6)) WHERE id=? AND status='QUEUED'`, id)
		if err != nil {
			return Job{}, false, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return Job{}, false, err
		}
		if rows == 0 {
			continue
		}
		job, err := repository.Get(ctx, id, 0, true)
		return job, err == nil, err
	}
}

func (repository *Repository) Pending(ctx context.Context, jobID, after string, limit int) ([]string, error) {
	var accounts []string
	err := repository.db.SelectContext(ctx, &accounts, `SELECT requested_account FROM slik_job_accounts
	 WHERE job_id=? AND status='QUEUED' AND requested_account > ? ORDER BY requested_account LIMIT ?`, jobID, after, limit)
	return accounts, err
}

func (repository *Repository) Save(ctx context.Context, jobID, account, primary string, values Values) error {
	transaction, err := repository.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	progress, err := transaction.ExecContext(ctx, `UPDATE slik_jobs SET processed_accounts=processed_accounts+1 WHERE id=? AND status='PROCESSING'`, jobID)
	if err != nil {
		return err
	}
	progressChanged, err := progress.RowsAffected()
	if err != nil {
		return err
	}
	if progressChanged != 1 {
		return errors.New("SLIK job is no longer processing")
	}
	result, err := transaction.ExecContext(ctx, `UPDATE slik_job_accounts SET status='COMPLETED', resolved_primary_account=?,
	 bakidebet=?, sukubungaimbalan=?, processed_at=UTC_TIMESTAMP(6)
	 WHERE job_id=? AND requested_account=? AND status='QUEUED'`, primary, values.Balance, values.Rate, jobID, account)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("SLIK checkpoint already processed or missing")
	}
	return transaction.Commit()
}

func (repository *Repository) Results(ctx context.Context, jobID string) (map[string]Values, error) {
	results := make(map[string]Values)
	var after string
	for {
		var rows []Checkpoint
		err := repository.db.SelectContext(ctx, &rows, `SELECT requested_account, bakidebet, sukubungaimbalan FROM slik_job_accounts
		 WHERE job_id=? AND status='COMPLETED' AND requested_account > ? ORDER BY requested_account LIMIT 256`, jobID, after)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return results, nil
		}
		for _, row := range rows {
			results[row.Account] = Values{row.Balance, row.Rate}
		}
		after = rows[len(rows)-1].Account
	}
}

func (repository *Repository) Terminal(ctx context.Context, jobID, status, account, reason, output string) error {
	if status != "COMPLETED" && status != "FAILED" && status != "CANCELED" {
		return errors.New("invalid terminal SLIK status")
	}
	result, err := repository.db.ExecContext(ctx, `UPDATE slik_jobs SET status=?, failed_account=NULLIF(?,''), failure_reason=NULLIF(?,''),
	 output_file=NULLIF(?,''), finished_at=UTC_TIMESTAMP(6), expires_at=DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 7 DAY)
	 WHERE id=? AND status='PROCESSING'`, status, account, reason, output, jobID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return errors.New("SLIK job was canceled or no longer processing")
	}
	return nil
}

func (repository *Repository) Cancel(ctx context.Context, id string, ownerID uint64, viewAll bool) error {
	query := `UPDATE slik_jobs SET status='CANCELED', finished_at=UTC_TIMESTAMP(6), expires_at=DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 7 DAY)
	 WHERE id=? AND status IN ('QUEUED','PROCESSING')`
	args := []any{id}
	if !viewAll {
		query += ` AND owner_user_id=?`
		args = append(args, ownerID)
	}
	if _, err := repository.db.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	_, err := repository.Get(ctx, id, ownerID, viewAll)
	return err
}

func (repository *Repository) IsCanceled(ctx context.Context, id string) (bool, error) {
	var status string
	err := repository.db.GetContext(ctx, &status, `SELECT status FROM slik_jobs WHERE id=?`, id)
	return status == "CANCELED", err
}

func (repository *Repository) Expired(ctx context.Context) ([]Job, error) {
	var jobs []Job
	err := repository.db.SelectContext(ctx, &jobs, `SELECT `+jobColumns+` FROM slik_jobs
	 WHERE status IN ('COMPLETED','FAILED','CANCELED') AND expires_at <= UTC_TIMESTAMP(6)
	 AND (input_file <> '' OR output_file IS NOT NULL) LIMIT 100`)
	return jobs, err
}

func (repository *Repository) ClearFiles(ctx context.Context, id string) error {
	_, err := repository.db.ExecContext(ctx, `UPDATE slik_jobs SET input_file='', output_file=NULL WHERE id=? AND expires_at <= UTC_TIMESTAMP(6)
	 AND status IN ('COMPLETED','FAILED','CANCELED')`, id)
	return err
}

func (repository *Repository) AcquireLock(ctx context.Context) (func(), <-chan struct{}, bool, error) {
	acquireContext, stopAcquire := context.WithTimeout(ctx, 5*time.Second)
	defer stopAcquire()
	connection, err := repository.db.Connx(acquireContext)
	if err != nil {
		return nil, nil, false, err
	}
	var acquired sql.NullInt64
	if err := connection.GetContext(acquireContext, &acquired, `SELECT GET_LOCK('trs:slik-job-worker', 0)`); err != nil {
		connection.Close()
		return nil, nil, false, err
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		connection.Close()
		return nil, nil, false, nil
	}
	lost, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				check, cancel := context.WithTimeout(ctx, 5*time.Second)
				var ownership struct {
					ConnectionID int64         `db:"connection_id"`
					LockOwner    sql.NullInt64 `db:"lock_owner"`
				}
				err := connection.GetContext(check, &ownership, `SELECT CONNECTION_ID() AS connection_id, IS_USED_LOCK('trs:slik-job-worker') AS lock_owner`)
				cancel()
				if err != nil || !ownership.LockOwner.Valid || ownership.ConnectionID != ownership.LockOwner.Int64 {
					close(lost)
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = connection.ExecContext(cleanup, `SELECT RELEASE_LOCK('trs:slik-job-worker')`)
			_ = connection.Close()
		})
	}, lost, true, nil
}

func (repository *Repository) CheckComplete(ctx context.Context, id string) error {
	var remaining int
	if err := repository.db.GetContext(ctx, &remaining, `SELECT COUNT(*) FROM slik_job_accounts WHERE job_id=? AND status<>'COMPLETED'`, id); err != nil {
		return err
	}
	if remaining != 0 {
		return fmt.Errorf("%d SLIK accounts remain unprocessed", remaining)
	}
	return nil
}
