//go:build integration

package slik

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/testutil/integrationdb"
)

func TestRepositoryPersistsAndRecoversCheckpoints(t *testing.T) {
	db := integrationdb.Open(t)
	integrationdb.Reset(t, db, nil)
	owner := integrationdb.User(t, db, "slik-owner", true)
	repository := NewRepository(db)
	ctx := context.Background()
	release, _, acquired, err := repository.AcquireLock(ctx)
	if err != nil || !acquired {
		t.Fatalf("first global lock: acquired=%v err=%v", acquired, err)
	}
	if _, _, acquired, err := repository.AcquireLock(ctx); err != nil || acquired {
		t.Fatalf("duplicate global lock: acquired=%v err=%v", acquired, err)
	}
	release()
	job := Job{ID: "0123456789abcdef0123456789abcdef", OwnerID: owner.ID, OwnerUsername: owner.Username, ActorID: owner.ID, ActorUsername: owner.Username, OriginalFilename: "slik.xlsx", AsOf: "2026-08-31", TotalRows: 4, CreatedAt: time.Now().UTC(), InputFile: "0123456789abcdef0123456789abcdef.input.xlsx"}
	if err := repository.Create(ctx, job, []string{"00123", "other", "third"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(ctx, job.ID, owner.ID+1, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner access: %v", err)
	}
	history, err := repository.History(ctx, owner.ID, false)
	if err != nil || len(history) != 1 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	claimed, found, err := repository.ClaimNext(ctx)
	if err != nil || !found || claimed.ID != job.ID {
		t.Fatalf("claim=%+v found=%v err=%v", claimed, found, err)
	}
	if _, found, err := repository.ClaimNext(ctx); err != nil || found {
		t.Fatalf("duplicate claim: found=%v err=%v", found, err)
	}
	if err := repository.Save(ctx, job.ID, "00123", "primary", Values{"12.34", "7.89123456789"}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Save(ctx, job.ID, "other", "other", Values{"0.00", "1.5"}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	claimed, found, err = repository.ClaimNext(ctx)
	if err != nil || !found || claimed.Processed != 2 {
		t.Fatalf("reclaim=%+v found=%v err=%v", claimed, found, err)
	}
	remaining, err := repository.Pending(ctx, job.ID, "", 100)
	if err != nil || len(remaining) != 1 || remaining[0] != "third" {
		t.Fatalf("remaining=%v err=%v", remaining, err)
	}
	if err := repository.Save(ctx, job.ID, "third", "third", Values{"1.00", "2.5"}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CheckComplete(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	results, err := repository.Results(ctx, job.ID)
	if err != nil || results["00123"].Rate != "7.89123456789" {
		t.Fatalf("results=%v err=%v", results, err)
	}
	if err := repository.Terminal(ctx, job.ID, "COMPLETED", "", "", job.ID+".output.xlsx"); err != nil {
		t.Fatal(err)
	}
	if err := repository.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	completed, err := repository.Get(ctx, job.ID, owner.ID, false)
	if err != nil || completed.Status != "COMPLETED" || completed.Processed != 3 || completed.ExpiresAt == nil {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	if err := repository.Cancel(ctx, job.ID, owner.ID, false); err != nil {
		t.Fatal(err)
	}
	completed, _ = repository.Get(ctx, job.ID, owner.ID, false)
	if completed.Status != "COMPLETED" {
		t.Fatalf("terminal job canceled: %+v", completed)
	}
	if _, err := db.Exec(`UPDATE slik_jobs SET expires_at=UTC_TIMESTAMP(6) - INTERVAL 1 SECOND WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := repository.Expired(ctx)
	if err != nil || len(expired) != 1 {
		t.Fatalf("expired=%+v err=%v", expired, err)
	}
	if err := repository.ClearFiles(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	completed, _ = repository.Get(ctx, job.ID, owner.ID, false)
	if completed.OutputFile != "" || completed.InputFile != "" {
		t.Fatalf("file references retained: %+v", completed)
	}
	other := integrationdb.User(t, db, "slik-other", true)
	otherJob := Job{ID: "fedcba9876543210fedcba9876543210", OwnerID: other.ID, OwnerUsername: other.Username, ActorID: other.ID, ActorUsername: other.Username, OriginalFilename: "other.xlsx", AsOf: "2026-08-31", CreatedAt: time.Now().UTC(), InputFile: "other.input.xlsx"}
	if err := repository.Create(ctx, otherJob, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(ctx, otherJob.ID, owner.ID, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary user opened another owner's job: %v", err)
	}
	if visible, err := repository.Get(ctx, otherJob.ID, owner.ID, true); err != nil || visible.OwnerID != other.ID {
		t.Fatalf("view-all access: job=%+v err=%v", visible, err)
	}
	if own, err := repository.History(ctx, owner.ID, false); err != nil || len(own) != 1 {
		t.Fatalf("owner history=%v err=%v", own, err)
	}
	if all, err := repository.History(ctx, owner.ID, true); err != nil || len(all) != 2 {
		t.Fatalf("view-all history=%v err=%v", all, err)
	}
}
