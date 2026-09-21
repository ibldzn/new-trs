package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ibldzn/trs/internal/loan"
)

const refreshLockName = "trs:current-loan-snapshot-refresh"

type Repository struct {
	database *sqlx.DB
	location *time.Location
}

func NewRepository(database *sqlx.DB, location *time.Location) *Repository {
	return &Repository{database: database, location: location}
}

func (repository *Repository) ExactPosition(ctx context.Context, account string, asOf loan.Date) (loan.LoanPosition, error) {
	const query = `
		SELECT account_number, as_of_date, loan_start_date, principal_outstanding, collectability_bi,
		       principal_arrears, interest_arrears, penalty_arrears, branch, product, cif, contract_number, source_updated_at
		FROM current_loan_position_snapshot
		WHERE account_number = ? AND as_of_date = ?`
	var row struct {
		AccountNumber        string         `db:"account_number"`
		AsOf                 time.Time      `db:"as_of_date"`
		LoanStartDate        sql.NullTime   `db:"loan_start_date"`
		PrincipalOutstanding loan.Money     `db:"principal_outstanding"`
		CollectabilityBI     int            `db:"collectability_bi"`
		PrincipalDue         loan.Money     `db:"principal_arrears"`
		InterestDue          loan.Money     `db:"interest_arrears"`
		PenaltyDue           loan.Money     `db:"penalty_arrears"`
		Branch               sql.NullString `db:"branch"`
		Product              sql.NullString `db:"product"`
		CIF                  sql.NullString `db:"cif"`
		ContractNumber       sql.NullString `db:"contract_number"`
		SourceUpdatedAt      sql.NullTime   `db:"source_updated_at"`
	}
	if err := repository.database.GetContext(ctx, &row, query, strings.TrimSpace(account), asOf.String()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return loan.LoanPosition{}, errors.Join(loan.ErrNotFound, loan.ErrCurrentSnapshot)
		}
		return loan.LoanPosition{}, errors.Join(loan.ErrCurrentSnapshot, err)
	}
	if row.CollectabilityBI < 1 || row.CollectabilityBI > 5 || row.PrincipalOutstanding.IsNegative() || row.PrincipalDue.IsNegative() || row.InterestDue.IsNegative() || row.PenaltyDue.IsNegative() || row.PrincipalDue.Cmp(row.PrincipalOutstanding) > 0 {
		return loan.LoanPosition{}, fmt.Errorf("%w: invalid stored position", loan.ErrCurrentSnapshot)
	}
	if !row.LoanStartDate.Valid || row.LoanStartDate.Time.IsZero() {
		return loan.LoanPosition{}, errors.Join(loan.ErrCurrentSnapshot, loan.ErrHistoricalEvidence, fmt.Errorf("snapshot loan start date is missing"))
	}
	loanStartDate, err := loan.ParseDate(row.LoanStartDate.Time.Format(loan.DateLayout), repository.location)
	if err != nil || loanStartDate.After(asOf) {
		return loan.LoanPosition{}, errors.Join(loan.ErrCurrentSnapshot, loan.ErrHistoricalEvidence, fmt.Errorf("invalid snapshot loan start date"))
	}
	position := loan.LoanPosition{
		AsOf: loan.NewDate(row.AsOf, repository.location), LoanStartDate: loanStartDate, AccountNumber: row.AccountNumber,
		PrincipalOutstanding: row.PrincipalOutstanding, PrincipalDue: row.PrincipalDue, InterestDue: row.InterestDue, PenaltyDue: row.PenaltyDue,
		CollectabilityBI: row.CollectabilityBI, Source: loan.SourceTodaySnapshot,
		Branch: row.Branch.String, Product: row.Product.String, CIF: row.CIF.String, ContractNumber: row.ContractNumber.String,
	}
	if row.SourceUpdatedAt.Valid {
		value := row.SourceUpdatedAt.Time
		position.SourceUpdatedAt = &value
	}
	return position, nil
}

func (repository *Repository) ReplaceAll(ctx context.Context, rows []loan.LoanPosition, refreshedAt time.Time) error {
	transaction, err := repository.database.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin snapshot replacement: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `DELETE FROM current_loan_position_snapshot`); err != nil {
		return fmt.Errorf("clear current snapshot: %w", err)
	}
	const chunkSize = 200
	for start := 0; start < len(rows); start += chunkSize {
		end := min(start+chunkSize, len(rows))
		if err := insertRows(ctx, transaction, rows[start:end], refreshedAt.UTC()); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx, `UPDATE snapshot_refresh_state SET last_successful_at=?, last_status='succeeded', last_row_count=?, last_error_summary=NULL, updated_at=? WHERE id=1`, refreshedAt.UTC(), len(rows), refreshedAt.UTC()); err != nil {
		return fmt.Errorf("record snapshot success: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit snapshot replacement: %w", err)
	}
	return nil
}

func insertRows(ctx context.Context, transaction *sqlx.Tx, rows []loan.LoanPosition, refreshedAt time.Time) error {
	const columns = `(account_number, as_of_date, loan_start_date, principal_outstanding, collectability_bi, principal_arrears, interest_arrears, penalty_arrears, branch, product, cif, contract_number, source_updated_at, refreshed_at)`
	values := make([]string, 0, len(rows))
	arguments := make([]any, 0, len(rows)*14)
	for _, row := range rows {
		if row.LoanStartDate.IsZero() || row.LoanStartDate.After(row.AsOf) {
			return fmt.Errorf("%w: invalid snapshot loan start date", loan.ErrHistoricalEvidence)
		}
		if row.PenaltyDue.IsNegative() {
			return fmt.Errorf("%w: invalid snapshot penalty arrears", loan.ErrCurrentSnapshot)
		}
		values = append(values, `(?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`)
		arguments = append(arguments, row.AccountNumber, row.AsOf.String(), row.LoanStartDate.String(), row.PrincipalOutstanding, row.CollectabilityBI,
			row.PrincipalDue, row.InterestDue, row.PenaltyDue, row.Branch, row.Product, row.CIF, row.ContractNumber, row.SourceUpdatedAt, refreshedAt)
	}
	query := `INSERT INTO current_loan_position_snapshot ` + columns + ` VALUES ` + strings.Join(values, ",")
	if _, err := transaction.ExecContext(ctx, query, arguments...); err != nil {
		return fmt.Errorf("insert snapshot rows: %w", err)
	}
	return nil
}

func (repository *Repository) Status(ctx context.Context) (Status, error) {
	var status Status
	if err := repository.database.GetContext(ctx, &status, `SELECT last_attempted_at, last_successful_at, last_status, last_row_count, last_error_summary FROM snapshot_refresh_state WHERE id = 1`); err != nil {
		return Status{}, fmt.Errorf("read snapshot status: %w", err)
	}
	return status, nil
}

func (repository *Repository) MarkAttempt(ctx context.Context, now time.Time) error {
	_, err := repository.database.ExecContext(ctx, `UPDATE snapshot_refresh_state SET last_attempted_at=?, last_status='running', last_error_summary=NULL, updated_at=? WHERE id=1`, now.UTC(), now.UTC())
	return err
}

func (repository *Repository) MarkFailure(ctx context.Context, now time.Time, summary string) error {
	_, err := repository.database.ExecContext(ctx, `UPDATE snapshot_refresh_state SET last_status='failed', last_error_summary=?, updated_at=? WHERE id=1`, summary, now.UTC())
	return err
}

func (repository *Repository) AcquireRefreshLock(ctx context.Context) (func(), bool, error) {
	connection, err := repository.database.Connx(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("open snapshot lock connection: %w", err)
	}
	var acquired sql.NullInt64
	if err := connection.GetContext(ctx, &acquired, `SELECT GET_LOCK(?, 0)`, refreshLockName); err != nil {
		_ = connection.Close()
		return nil, false, fmt.Errorf("acquire snapshot lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		_ = connection.Close()
		return nil, false, nil
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			releaseContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = connection.ExecContext(releaseContext, `SELECT RELEASE_LOCK(?)`, refreshLockName)
			cancel()
			_ = connection.Close()
		})
	}
	return release, true, nil
}
