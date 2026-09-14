package fincloud

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/contractual"
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

func TestNormalizeContractScheduleIgnoresInstallmentZeroAndSorts(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	schedule, err := normalizeContractSchedule([]scheduleDTO{
		{Date: "2026-03-01", InstallmentNo: 2},
		{Date: "2026-01-01", InstallmentNo: 0},
		{Date: "2026-04-01", InstallmentNo: 3},
		{Date: "2026-02-01", InstallmentNo: 1},
	}, 3, location)
	if err != nil {
		t.Fatal(err)
	}
	if len(schedule) != 3 {
		t.Fatalf("contractual schedule count = %d", len(schedule))
	}
	for index, wantDate := range []string{"2026-02-01", "2026-03-01", "2026-04-01"} {
		if schedule[index].Number != index+1 || schedule[index].DueDate.String() != wantDate {
			t.Fatalf("schedule[%d] = %+v", index, schedule[index])
		}
	}
}

func TestNormalizeContractScheduleRejectsInvalidEvidence(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	valid := []scheduleDTO{{Date: "2026-01-01", InstallmentNo: 0}, {Date: "2026-02-01", InstallmentNo: 1}, {Date: "2026-03-01", InstallmentNo: 2}, {Date: "2026-04-01", InstallmentNo: 3}}
	for _, test := range []struct {
		name string
		rows []scheduleDTO
	}{
		{name: "duplicate installment", rows: append(append([]scheduleDTO(nil), valid...), scheduleDTO{Date: "2026-02-02", InstallmentNo: 1})},
		{name: "missing installment", rows: []scheduleDTO{{Date: "2026-01-01", InstallmentNo: 0}, {Date: "2026-02-01", InstallmentNo: 1}, {Date: "2026-04-01", InstallmentNo: 3}}},
		{name: "installment over tenor", rows: append(append([]scheduleDTO(nil), valid...), scheduleDTO{Date: "2026-05-01", InstallmentNo: 4})},
		{name: "negative installment", rows: append(append([]scheduleDTO(nil), valid...), scheduleDTO{Date: "2025-12-01", InstallmentNo: -1})},
		{name: "duplicate date", rows: []scheduleDTO{{Date: "2026-01-01", InstallmentNo: 0}, {Date: "2026-02-01", InstallmentNo: 1}, {Date: "2026-02-01", InstallmentNo: 2}, {Date: "2026-04-01", InstallmentNo: 3}}},
		{name: "non chronological date", rows: []scheduleDTO{{Date: "2026-01-01", InstallmentNo: 0}, {Date: "2026-03-01", InstallmentNo: 1}, {Date: "2026-02-01", InstallmentNo: 2}, {Date: "2026-04-01", InstallmentNo: 3}}},
		{name: "invalid date", rows: []scheduleDTO{{Date: "2026-01-01", InstallmentNo: 0}, {Date: "bad", InstallmentNo: 1}, {Date: "2026-03-01", InstallmentNo: 2}, {Date: "2026-04-01", InstallmentNo: 3}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeContractSchedule(test.rows, 3, location)
			if !errors.Is(err, loan.ErrUnsupportedCalculation) {
				t.Fatalf("error = %v", err)
			}
		})
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
	calculator := contractual.Calculator{Round: func(value loan.Money) loan.Money { return loan.MustMoney(value.Format(2)) }}
	rows, err := calculator.BuildSchedule(contract.PlafondLimit, contract.TenorMonths, contract.FlatRatePercent, contract.ContractSchedule)
	if err != nil {
		t.Fatal(err)
	}
	if contract.RawScheduleCount != 4 || len(rows) != 3 || rows[0].Number != 1 || rows[0].DueDate.String() != "2026-02-01" || rows[0].Principal.Format(2) != "33.33" || rows[0].Interest.Format(2) != "1.00" {
		t.Fatalf("raw=%d schedule=%+v", contract.RawScheduleCount, rows)
	}
}
