package fincloud

import (
	"encoding/json"
	"testing"
	"time"
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

func TestMapLoanUsesActualRestructuringFields(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata string
		changed  bool
	}{
		{name: "unchanged", metadata: `"restruktur_noakad_akhir":"","restruktur_tanggalakhirakad":null,"restruktur_tanggalawal":"0","restruktur_tanggalakhir":"false","restruktur_cara":"tidak","restruktur_frekuensi":"-","restrukturisasi":"1"`},
		{name: "boolean false", metadata: `"restruktur_cara":false`},
		{name: "final agreement", metadata: `"restruktur_noakad_akhir":"AKAD-R-2026-01"`, changed: true},
		{name: "agreement end date object", metadata: `"restruktur_tanggalakhirakad":{"date":"2030-06-30 00:00:00.000000","timezone_type":3,"timezone":"Asia/Jakarta"}`, changed: true},
		{name: "restructure start", metadata: `"restruktur_tanggalawal":"2026-01-15"`, changed: true},
		{name: "restructure end", metadata: `"restruktur_tanggalakhir":"2028-01-15"`, changed: true},
		{name: "method", metadata: `"restruktur_cara":"Perpanjangan tenor"`, changed: true},
		{name: "frequency", metadata: `"restruktur_frekuensi":2`, changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var source loanDTO
			raw := `{"id":"primary","plafondlimit":"100","jangkawaktu":"3 bulan","bungaflat":"12",` + test.metadata + `}`
			if err := json.Unmarshal([]byte(raw), &source); err != nil {
				t.Fatal(err)
			}
			contract, err := mapLoan(source, time.FixedZone("Jakarta", 7*60*60))
			if err != nil {
				t.Fatal(err)
			}
			if contract.ContractChanged != test.changed {
				t.Fatalf("ContractChanged = %t", contract.ContractChanged)
			}
		})
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
