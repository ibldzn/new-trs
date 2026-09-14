package dwh

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
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

type positionRow struct {
	AsOf                 time.Time  `db:"as_of_date"`
	AccountNumber        string     `db:"no_rekening"`
	PrincipalOutstanding loan.Money `db:"sisa_pokok_pinjaman"`
	CollectabilityBI     int        `db:"kolektibilitas_bi"`
	PrincipalDue         loan.Money `db:"tunggakan_pokok"`
	InterestDue          loan.Money `db:"tunggakan_bunga"`
}

func (repository *Repository) ExactPosition(ctx context.Context, account string, asOf loan.Date) (loan.LoanPosition, error) {
	const query = `
		SELECT as_of_date, no_rekening, sisa_pokok_pinjaman, kolektibilitas_bi, tunggakan_pokok, tunggakan_bunga
		FROM dwhv2.fincloud_eod_detail_outstanding_rekening_pinjaman
		WHERE as_of_date = ? AND business_key_hash = ?
		LIMIT 1`
	var row positionRow
	if err := repository.database.GetContext(ctx, &row, query, asOf.String(), accountBusinessKey(account)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return loan.LoanPosition{}, errors.Join(loan.ErrNotFound, loan.ErrHistoricalEvidence)
		}
		return loan.LoanPosition{}, errors.Join(loan.ErrDWHUnavailable, err)
	}
	if err := validateBalances(row.PrincipalOutstanding, row.PrincipalDue, row.InterestDue, row.CollectabilityBI); err != nil {
		return loan.LoanPosition{}, err
	}
	return loan.LoanPosition{
		AsOf: loan.NewDate(row.AsOf, repository.location), AccountNumber: strings.TrimSpace(row.AccountNumber),
		PrincipalOutstanding: row.PrincipalOutstanding, PrincipalDue: row.PrincipalDue, InterestDue: row.InterestDue,
		CollectabilityBI: row.CollectabilityBI, Source: loan.SourceDWH,
	}, nil
}

func (repository *Repository) CollectabilityTimeline(ctx context.Context, account string, from, to loan.Date) ([]loan.CollectabilityPoint, error) {
	if to.Before(from) {
		return []loan.CollectabilityPoint{}, nil
	}
	const query = `
		WITH RECURSIVE dates (as_of_date) AS (
			SELECT CAST(? AS DATE)
			UNION ALL
			SELECT DATE_ADD(as_of_date, INTERVAL 1 DAY)
			FROM dates
			WHERE as_of_date < CAST(? AS DATE)
		)
		SELECT position.as_of_date, position.kolektibilitas_bi
		FROM dates
		JOIN dwhv2.fincloud_eod_detail_outstanding_rekening_pinjaman AS position
		  ON position.as_of_date = dates.as_of_date
		 AND position.business_key_hash = ?
		ORDER BY position.as_of_date ASC`
	var rows []struct {
		Date  time.Time `db:"as_of_date"`
		Value int       `db:"kolektibilitas_bi"`
	}
	if err := repository.database.SelectContext(ctx, &rows, query, from.String(), to.String(), accountBusinessKey(account)); err != nil {
		return nil, errors.Join(loan.ErrDWHUnavailable, err)
	}
	points := make([]loan.CollectabilityPoint, 0, len(rows))
	for _, row := range rows {
		if row.Value < 1 || row.Value > 5 {
			return nil, fmt.Errorf("%w: DWH collectability outside 1..5", loan.ErrHistoricalEvidence)
		}
		point := loan.CollectabilityPoint{Date: loan.NewDate(row.Date, repository.location), Value: row.Value}
		if len(points) != 0 && points[len(points)-1].Date.Equal(point.Date) {
			if points[len(points)-1].Value != point.Value {
				return nil, fmt.Errorf("%w: conflicting DWH collectability on %s", loan.ErrHistoricalEvidence, point.Date)
			}
			continue
		}
		points = append(points, point)
	}
	return points, nil
}

func accountBusinessKey(account string) string {
	encoded, _ := json.Marshal([]string{strings.TrimSpace(account)})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func validateBalances(principal, principalDue, interestDue loan.Money, collectability int) error {
	if principal.IsNegative() || principalDue.IsNegative() || interestDue.IsNegative() || principalDue.Cmp(principal) > 0 || collectability < 1 || collectability > 5 {
		return fmt.Errorf("%w: invalid DWH loan position", loan.ErrHistoricalEvidence)
	}
	return nil
}
