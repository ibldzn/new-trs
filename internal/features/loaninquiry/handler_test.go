package loaninquiry

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

func TestInquiryErrorDoesNotMisreportMissingEvidenceAsMissingLoan(t *testing.T) {
	status, message := inquiryError(errors.Join(loan.ErrNotFound, loan.ErrHistoricalEvidence))
	if status != http.StatusServiceUnavailable || message != "Required historical evidence is unavailable." {
		t.Fatalf("status=%d message=%q", status, message)
	}
	status, _ = inquiryError(loan.ErrNotFound)
	if status != http.StatusNotFound {
		t.Fatalf("loan not-found status=%d", status)
	}
}

func TestResultViewExposesPrecomputedContractualScheduleForReconstructedLoan(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	due1, _ := loan.ParseDate("2026-02-01", location)
	due2, _ := loan.ParseDate("2026-03-01", location)
	due3, _ := loan.ParseDate("2026-04-01", location)
	resolved := loan.ResolvedPosition{
		Loan:     loan.ContractData{PlafondLimit: loan.MustMoney("100"), TenorMonths: 3, FlatRatePercent: loan.MustMoney("12"), ContractSchedule: []loan.ContractualInstallment{{Number: 1, DueDate: due1}, {Number: 2, DueDate: due2}, {Number: 3, DueDate: due3}}},
		Position: loan.LoanPosition{Source: loan.SourceReconstructed},
		ContractualSchedule: []loan.ContractualScheduleRow{
			{Number: 1, DueDate: due1, Principal: loan.MustMoney("33.33"), Interest: loan.MustMoney("1"), Installment: loan.MustMoney("34.33"), ScheduledBalance: loan.MustMoney("66.67")},
			{Number: 2, DueDate: due2, Principal: loan.MustMoney("33.33"), Interest: loan.MustMoney("1"), Installment: loan.MustMoney("34.33"), ScheduledBalance: loan.MustMoney("33.34")},
			{Number: 3, DueDate: due3, Principal: loan.MustMoney("33.34"), Interest: loan.MustMoney("1"), Installment: loan.MustMoney("34.34"), ScheduledBalance: loan.Money{}},
		},
	}
	view, err := newResultView(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if view.DueDateCount != 3 || len(view.ContractualSchedule) != 3 {
		t.Fatalf("count=%d rows=%d", view.DueDateCount, len(view.ContractualSchedule))
	}
	row := view.ContractualSchedule[2]
	if row.Number != 3 || row.DueDate != "01/04/2026" || row.Principal != "33.34" || row.Interest != "1.00" || row.Installment != "34.34" || row.ScheduledBalance != "0.00" {
		t.Fatalf("row = %+v", row)
	}

	resolved.Position.Source = loan.SourceDWH
	view, err = newResultView(resolved)
	if err != nil || len(view.ContractualSchedule) != 0 {
		t.Fatalf("non-reconstructed rows=%d error=%v", len(view.ContractualSchedule), err)
	}
}
