package mso

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/ibldzn/trs/internal/loan"
)

type Repository struct {
	database *sqlx.DB
	location *time.Location
}

func NewRepository(database *sqlx.DB, location *time.Location) *Repository {
	return &Repository{database: database, location: location}
}

func (repository *Repository) DebtorTypeByAlternateCIF(ctx context.Context, cif string) (string, error) {
	const query = `
		SELECT
			debitur_golongan2 AS debtor_type
		FROM
			data_nasabah_badan
		WHERE
			REPLACE(REPLACE(nasabah_master, '.', ''), '#', '') = ?`
	var debtorType string
	if err := repository.database.GetContext(ctx, &debtorType, query, cif); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", loan.ErrNotFound
		}
		return "", errors.Join(loan.ErrMSOUnavailable, err)
	}
	return debtorType, nil
}

type stateRow struct {
	PrincipalOutstanding loan.Money `db:"principal_outstanding"`
	PrincipalDue         loan.Money `db:"principal_due"`
	InterestDue          loan.Money `db:"interest_due"`
	PenaltyDue           loan.Money `db:"penalty_due"`
	CollectabilityBI     int        `db:"-"`
	CollectabilityCode   string     `db:"collectability_bi"`
}

func (repository *Repository) HistoricalPosition(ctx context.Context, account string, asOf loan.Date) (loan.LoanPosition, error) {
	row, err := repository.state(ctx, account, asOf)
	if err != nil {
		return loan.LoanPosition{}, err
	}
	return loan.LoanPosition{
		AsOf: asOf, AccountNumber: strings.TrimSpace(account), PrincipalOutstanding: row.PrincipalOutstanding,
		PrincipalDue: row.PrincipalDue, InterestDue: row.InterestDue, PenaltyDue: row.PenaltyDue,
		CollectabilityBI: row.CollectabilityBI, Source: loan.SourceMSO,
	}, nil
}

func (repository *Repository) OpeningState(ctx context.Context, account string, cutoff loan.Date) (loan.OpeningLoanState, error) {
	row, err := repository.state(ctx, account, cutoff)
	if err != nil {
		return loan.OpeningLoanState{}, err
	}
	const query = `
		SELECT
			kre_sistem_bunga AS interest_type
		FROM
			data_kredit_master
		WHERE
			kre_rekening = ?`
	var interestType string
	if err := repository.database.GetContext(ctx, &interestType, query, strings.TrimSpace(account)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return loan.OpeningLoanState{}, errors.Join(loan.ErrNotFound, loan.ErrHistoricalEvidence)
		}
		return loan.OpeningLoanState{}, errors.Join(loan.ErrMSOUnavailable, fmt.Errorf("read interest type: %w", err))
	}
	return loan.OpeningLoanState{
		AccountNumber: strings.TrimSpace(account), InterestType: strings.TrimSpace(interestType),
		PrincipalOutstanding: row.PrincipalOutstanding, PrincipalDue: row.PrincipalDue,
		InterestDue: row.InterestDue, PenaltyDue: row.PenaltyDue, CollectabilityBI: row.CollectabilityBI,
	}, nil
}

func (repository *Repository) state(ctx context.Context, account string, asOf loan.Date) (stateRow, error) {
	const query = `
		SELECT
			HitungKreditBakiDebet(?, ?) AS principal_outstanding,
			HitungKreditTunggakPokok(?, ?) AS principal_due,
			HitungKreditTunggakBunga(?, ?) AS interest_due,
			HitungKreditDenda(?, ?) AS penalty_due,
			GetKreditKolek(?, ?) AS collectability_bi`
	account = strings.TrimSpace(account)
	date := asOf.String()
	var row stateRow
	if err := repository.database.GetContext(ctx, &row, query, account, date, account, date, account, date, account, date, account, date); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return stateRow{}, errors.Join(loan.ErrNotFound, loan.ErrHistoricalEvidence)
		}
		return stateRow{}, errors.Join(loan.ErrMSOUnavailable, err)
	}
	collectability, err := parseCollectabilityBI(row.CollectabilityCode)
	if err != nil {
		return stateRow{}, err
	}
	row.CollectabilityBI = collectability
	if row.PrincipalOutstanding.IsNegative() || row.PrincipalDue.IsNegative() || row.InterestDue.IsNegative() || row.PenaltyDue.IsNegative() || row.PrincipalDue.Cmp(row.PrincipalOutstanding) > 0 || row.CollectabilityBI < 1 || row.CollectabilityBI > 5 {
		return stateRow{}, fmt.Errorf("%w: invalid MSO loan position", loan.ErrHistoricalEvidence)
	}
	return row, nil
}

func parseCollectabilityBI(value string) (int, error) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "1", "L":
		return 1, nil
	case "2", "DPK", "DP":
		return 2, nil
	case "3", "KL":
		return 3, nil
	case "4", "D":
		return 4, nil
	case "5", "M":
		return 5, nil
	default:
		return 0, fmt.Errorf("%w: invalid MSO collectability %q", loan.ErrHistoricalEvidence, value)
	}
}
