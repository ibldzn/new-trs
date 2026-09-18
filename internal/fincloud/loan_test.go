package fincloud

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

func TestNormalizeDecimalPreservesSALAKSeparatorRules(t *testing.T) {
	for raw, want := range map[string]string{
		"1.234,56":     "1234.56",
		"1,234.56":     "1234.56",
		"1.234.567":    "1234567",
		"1234.567.890": "1234567.890",
		"1234.56":      "1234.56",
	} {
		got, err := NormalizeDecimal(raw)
		if err != nil || got != want {
			t.Fatalf("NormalizeDecimal(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := NormalizeDecimal("12x"); err == nil {
		t.Fatal("malformed decimal accepted")
	}
}

func TestMapLoanCloseDateForms(t *testing.T) {
	for _, test := range []struct {
		name, fields, wantDate, wantError string
	}{
		{"legacy string", `,"tgltutup":"2026-08-20"`, "2026-08-20", ""},
		{"live object", `,"tgl_tutup":{"date":"2026-08-20 00:00:00.000000","timezone_type":3,"timezone":"Asia/Jakarta"}`, "2026-08-20", ""},
		{"neither field despite closed status", "", "", ""},
		{"both same calendar date", `,"tgltutup":"2026-08-20","tgl_tutup":{"date":"2026-08-20 23:59:59.000000"}`, "2026-08-20", ""},
		{"empty object falls back to legacy", `,"tgltutup":"2026-08-20","tgl_tutup":{"date":""}`, "2026-08-20", ""},
		{"conflicting dates", `,"tgltutup":"2026-08-21","tgl_tutup":{"date":"2026-08-20 00:00:00.000000"}`, "", "conflicting tgltutup and tgl_tutup"},
		{"malformed object date", `,"tgl_tutup":{"date":"2026-02-30 00:00:00.000000"}`, "", "invalid tgl_tutup.date"},
		{"malformed object does not fall back", `,"tgltutup":"2026-08-20","tgl_tutup":{"date":"bad"}`, "", "invalid tgl_tutup.date"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var source loanDTO
			raw := `{"id":"primary","plafondlimit":"100","jangkawaktu":"1 bulan","bungaflat":"11.76","statusrekening":"Closed"` + test.fields + `}`
			if err := json.Unmarshal([]byte(raw), &source); err != nil {
				t.Fatal(err)
			}
			contract, err := mapLoan(source, time.FixedZone("Jakarta", 7*60*60))
			if test.wantError != "" {
				if !errors.Is(err, loan.ErrFincloudUnavailable) || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil || contract.CloseDate.String() != test.wantDate {
				t.Fatalf("close date=%s want=%s error=%v", contract.CloseDate, test.wantDate, err)
			}
		})
	}
}

func TestMapLoanIgnoresMisleadingFincloudDisbursementDate(t *testing.T) {
	var source loanDTO
	if err := json.Unmarshal([]byte(`{"id":"primary","plafondlimit":"100","jangkawaktu":"1 bulan","bungaflat":"12","tgl_pencairan":"2025-10-13"}`), &source); err != nil {
		t.Fatal(err)
	}
	contract, err := mapLoan(source, time.FixedZone("Jakarta", 7*60*60))
	if err != nil || contract.PrimaryAccount != "primary" {
		t.Fatalf("contract=%+v error=%v", contract, err)
	}
}

func TestMapLoanKeepsOnlyFincloudScheduleNumberAndDateAsContractualTruth(t *testing.T) {
	var source loanDTO
	err := json.Unmarshal([]byte(`{"id":"primary","plafondlimit":"100","jangkawaktu":"3 bulan","bungaflat":"12","jadwalangsuran":[{"angsuranke":0,"tanggal":"2026-01-01","pokok":"999"},{"angsuranke":1,"tanggal":"2026-02-01","pokok":"999","bunga":"999","angsuran":"999"},{"angsuranke":2,"tanggal":"2026-03-01","sisapinjaman":"999"},{"angsuranke":3,"tanggal":"2026-04-01","bayar_pokok":"999","bayar_bunga":"999"}]}`), &source)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := mapLoan(source, time.FixedZone("Jakarta", 7*60*60))
	if err != nil {
		t.Fatal(err)
	}
	if contract.RawScheduleCount != 4 || len(contract.ContractScheduleEvidence) != 4 || len(contract.ContractSchedule) != 0 {
		t.Fatalf("raw=%d evidence=%+v normalized=%+v", contract.RawScheduleCount, contract.ContractScheduleEvidence, contract.ContractSchedule)
	}
	for index, wantDate := range []string{"2026-01-01", "2026-02-01", "2026-03-01", "2026-04-01"} {
		if contract.ContractScheduleEvidence[index].Number != int64(index) || contract.ContractScheduleEvidence[index].RawDueDate != wantDate {
			t.Fatalf("evidence[%d] = %+v", index, contract.ContractScheduleEvidence[index])
		}
	}
}

func TestMapLoanIgnoresRestructuringMetadata(t *testing.T) {
	var source loanDTO
	raw := `{"id":"primary","plafondlimit":"100","jangkawaktu":"1 bulan","bungaflat":"12","restruktur_tanggalakhirakad":{"date":"2023-10-30 00:00:00.000000","timezone_type":3,"timezone":"Asia/Jakarta"},"jadwalangsuran":[{"angsuranke":1,"tanggal":"2025-11-12"}]}`
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		t.Fatal(err)
	}
	contract, err := mapLoan(source, time.FixedZone("Jakarta", 7*60*60))
	if err != nil {
		t.Fatal(err)
	}
	if contract.PrimaryAccount != "primary" || len(contract.ContractScheduleEvidence) != 1 {
		t.Fatalf("contract = %+v", contract)
	}
}

func TestMapLoanDefersInvalidScheduleEvidence(t *testing.T) {
	var source loanDTO
	if err := json.Unmarshal([]byte(`{"id":"primary","plafondlimit":"100","jangkawaktu":"2 bulan","bungaflat":"12","jadwalangsuran":[{"angsuranke":1,"tanggal":"bad"}]}`), &source); err != nil {
		t.Fatal(err)
	}
	contract, err := mapLoan(source, time.FixedZone("Jakarta", 7*60*60))
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.ContractScheduleEvidence) != 1 || contract.ContractScheduleEvidence[0].RawDueDate != "bad" {
		t.Fatalf("evidence = %+v", contract.ContractScheduleEvidence)
	}
}

func TestMapLoanPreservesClosedRateAndSignedRepaymentHistory(t *testing.T) {
	var source loanDTO
	raw := `{"id":"primary","noalt":"alternate","plafondlimit":"1000000","jangkawaktu":"12 Month","bungaflat":"18","tgltutup":"2026-08-31","historybayar":[{"tglbayar":"2026-08-20","bayar_pokok":"300,000.00","bayar_bunga":"0.00","bayar_denda":"0.00","bayar_dendapelunasan":"0.00","nominaldwp":"0.00","totalbayar":"300,000.00","nojurnal":"original"},{"tglbayar":"2026-08-20","bayar_pokok":"-300,000.00","bayar_bunga":"0.00","bayar_denda":"0.00","bayar_dendapelunasan":"0.00","nominaldwp":"0.00","totalbayar":"-300,000.00","nojurnal":"different"}]}`
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		t.Fatal(err)
	}
	contract, err := mapLoan(source, time.FixedZone("Jakarta", 7*60*60))
	if err != nil {
		t.Fatal(err)
	}
	if contract.CloseDate.String() != "2026-08-31" || contract.FlatRatePercent.Cmp(loan.MustMoney("18")) != 0 ||
		len(contract.Repayments) != 2 || contract.Repayments[0].PrincipalComponent.Cmp(loan.MustMoney("300000")) != 0 ||
		contract.Repayments[1].PrincipalComponent.Cmp(loan.MustMoney("-300000")) != 0 ||
		contract.Repayments[1].TotalPayment.Cmp(loan.MustMoney("-300000")) != 0 ||
		contract.Repayments[0].SourceOrder != 0 || contract.Repayments[1].SourceOrder != 1 ||
		contract.Repayments[0].JournalNumber == contract.Repayments[1].JournalNumber {
		t.Fatalf("closed mapping lost rate, date, signs, or order: %+v", contract)
	}
}
