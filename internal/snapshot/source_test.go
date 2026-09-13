package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

type reportStub struct {
	name   string
	params []string
	body   []byte
	err    error
}

func (stub *reportStub) DownloadNamedReport(_ context.Context, name string, params ...string) ([]byte, error) {
	stub.name = name
	stub.params = append([]string(nil), params...)
	return stub.body, stub.err
}

func TestSourceUsesSALAKReportContractAndMapsRows(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	report := &reportStub{body: []byte("\xEF\xBB\xBFDate Params|Branch Code|Product ID|Loan Account No|CIF No|Loan Agreement No|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears\n2026-09-14|001-JAKARTA|P1| 1001 |C1|K1|1.234,50|2|100,25|20.00\n")}
	source := NewSource(report, location)
	date, _ := loan.ParseDate("2026-09-14", location)
	rows, err := source.FetchCurrentLoanPositions(context.Background(), date)
	if err != nil {
		t.Fatal(err)
	}
	if report.name != todayOutstandingReport || len(report.params) != 1 || report.params[0] != "" {
		t.Fatalf("report=%q params=%v", report.name, report.params)
	}
	if len(rows) != 1 || rows[0].AccountNumber != "1001" || rows[0].Branch != "001" || rows[0].PrincipalOutstanding.Format(2) != "1234.50" || rows[0].PrincipalDue.Format(2) != "100.25" || rows[0].InterestDue.Format(2) != "20.00" || rows[0].CollectabilityBI != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestSourceRejectsIncompleteOrConflictingDataset(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	date, _ := loan.ParseDate("2026-09-14", location)
	for _, test := range []struct{ name, body string }{
		{"missing column", "Date Params|Loan Account No\n2026-09-14|1\n"},
		{"wrong date", "Date Params|Loan Account No|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears\n2026-09-13|1|10|1|0|0\n"},
		{"malformed money", "Date Params|Loan Account No|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears\n2026-09-14|1|bad|1|0|0\n"},
		{"conflicting duplicate", "Date Params|Loan Account No|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears\n2026-09-14|1|10|1|0|0\n2026-09-14|1|11|1|0|0\n"},
		{"empty", "Date Params|Loan Account No|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := NewSource(&reportStub{body: []byte(test.body)}, location)
			if _, err := source.FetchCurrentLoanPositions(context.Background(), date); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
