package loaninquiry

import (
	"errors"
	"net/http"
	"testing"

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
