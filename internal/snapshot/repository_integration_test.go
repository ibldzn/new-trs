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

func TestRepositoryReplacesSnapshotAndSerializesRefresh(t *testing.T) {
	database := integrationdb.Open(t)
	if _, err := database.Exec(`DELETE FROM current_loan_position_snapshot`); err != nil {
		t.Fatal(err)
	}
	repository := NewRepository(database, time.UTC)
	asOf, _ := loan.ParseDate("2026-09-14", time.UTC)
	oldRow := loan.LoanPosition{AsOf: asOf, AccountNumber: "old", PrincipalOutstanding: loan.MustMoney("10"), CollectabilityBI: 1}
	newRow := loan.LoanPosition{AsOf: asOf, AccountNumber: "new", PrincipalOutstanding: loan.MustMoney("20"), CollectabilityBI: 2}
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
	if err != nil || position.PrincipalOutstanding.Format(2) != "20.00" || position.CollectabilityBI != 2 {
		t.Fatalf("position=%+v error=%v", position, err)
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
