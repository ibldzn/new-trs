//go:build integration

package snapshot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
	"github.com/ibldzn/trs/internal/testutil/integrationdb"
)

func TestPenaltyArrearsSchemaDefaultsAndRejectsNegativeValues(t *testing.T) {
	database := integrationdb.Open(t)
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `DELETE FROM current_loan_position_snapshot`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO current_loan_position_snapshot
		(account_number, as_of_date, loan_start_date, principal_outstanding, collectability_bi, principal_arrears, interest_arrears, refreshed_at)
		VALUES ('existing', '2026-09-14', '2024-06-01', 20, 2, 2, 1, UTC_TIMESTAMP(6))`); err != nil {
		t.Fatal(err)
	}
	var penalty loan.Money
	if err := database.GetContext(ctx, &penalty, `SELECT penalty_arrears FROM current_loan_position_snapshot WHERE account_number = 'existing'`); err != nil || !penalty.IsZero() {
		t.Fatalf("backfilled penalty=%s error=%v", penalty, err)
	}
	if _, err := database.ExecContext(ctx, `UPDATE current_loan_position_snapshot SET penalty_arrears = -1 WHERE account_number = 'existing'`); err == nil {
		t.Fatal("negative penalty arrears passed database constraint")
	}
}

func TestRepositoryReplacesSnapshotAndSerializesRefresh(t *testing.T) {
	database := integrationdb.Open(t)
	if _, err := database.Exec(`DELETE FROM current_loan_position_snapshot`); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(database, time.UTC)
	asOf, _ := loan.ParseDate("2026-09-14", time.UTC)
	start, _ := loan.ParseDate("2024-06-01", time.UTC)
	oldRow := loan.LoanPosition{AsOf: asOf, LoanStartDate: start, AccountNumber: "old", PrincipalOutstanding: loan.MustMoney("10"), CollectabilityBI: 1}
	newRow := loan.LoanPosition{AsOf: asOf, LoanStartDate: start, AccountNumber: "new", PrincipalOutstanding: loan.MustMoney("20"), PrincipalDue: loan.MustMoney("2"), InterestDue: loan.MustMoney("1"), PenaltyDue: loan.MustMoney("25"), CollectabilityBI: 2, Branch: "001", Product: "P1", CIF: "C1", ContractNumber: "K1"}
	if err := repository.ReplaceAll(context.Background(), []loan.LoanPosition{oldRow}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceAll(context.Background(), []loan.LoanPosition{newRow}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ExactPosition(context.Background(), "old", asOf); !errors.Is(err, loan.ErrCurrentSnapshot) {
		t.Fatalf("stale row error = %v", err)
	}
	position, err := repository.ExactPosition(context.Background(), "new", asOf)
	if err != nil || position.LoanStartDate.String() != "2024-06-01" || position.PrincipalOutstanding.Format(2) != "20.00" || position.PrincipalDue.Format(2) != "2.00" || position.InterestDue.Format(2) != "1.00" || position.PenaltyDue.Format(2) != "25.00" || position.CollectabilityBI != 2 || position.Branch != "001" || position.Product != "P1" || position.CIF != "C1" || position.ContractNumber != "K1" {
		t.Fatalf("position=%+v error=%v", position, err)
	}
	invalid := newRow
	invalid.LoanStartDate = loan.Date{}
	if err := repository.ReplaceAll(context.Background(), []loan.LoanPosition{invalid}, time.Now()); !errors.Is(err, loan.ErrHistoricalEvidence) {
		t.Fatalf("invalid replacement error=%v", err)
	}
	if _, err := repository.ExactPosition(context.Background(), "new", asOf); err != nil {
		t.Fatalf("invalid replacement removed prior snapshot: %v", err)
	}
	invalid = newRow
	invalid.PenaltyDue = loan.MustMoney("-1")
	if err := repository.ReplaceAll(context.Background(), []loan.LoanPosition{invalid}, time.Now()); !errors.Is(err, loan.ErrCurrentSnapshot) {
		t.Fatalf("negative penalty replacement error=%v", err)
	}
	if _, err := repository.ExactPosition(context.Background(), "new", asOf); err != nil {
		t.Fatalf("negative penalty replacement removed prior snapshot: %v", err)
	}
	if _, err := database.Exec(`UPDATE current_loan_position_snapshot SET loan_start_date = NULL WHERE account_number = 'new'`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ExactPosition(context.Background(), "new", asOf); !errors.Is(err, loan.ErrHistoricalEvidence) {
		t.Fatalf("old nullable row error=%v", err)
	}
	status, err := repository.Status(context.Background())
	if err != nil || status.LastStatus != "succeeded" || status.LastRowCount != 1 {
		t.Fatalf("status=%+v error=%v", status, err)
	}
	release, acquired, err := repository.AcquireRefreshLock(context.Background())
	if err != nil || !acquired {
		t.Fatalf("first lock acquired=%v error=%v", acquired, err)
	}
	defer release()
	secondRelease, acquired, err := repository.AcquireRefreshLock(context.Background())
	if secondRelease != nil || acquired || err != nil {
		t.Fatalf("overlap lock acquired=%v error=%v", acquired, err)
	}
}
