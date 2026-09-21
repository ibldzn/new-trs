package snapshot

import (
	"context"
	"strings"
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
	report := &reportStub{body: []byte("\xEF\xBB\xBFdate_params|branch_code|product_id|loan_account_no|cif_no|loan_agreement_no|start_date|loan_outstanding|bi_collectability|principal_arrears|interest_arrears|penalty_arrears\n2026-09-14|001-JAKARTA|P1| 1001 |C1|K1|2024-06-01|1.234,50|2|100,25|20.00|5.50\n")}
	source := NewSource(report, location)
	date, _ := loan.ParseDate("2026-09-14", location)
	rows, err := source.FetchCurrentLoanPositions(context.Background(), date)
	if err != nil {
		t.Fatal(err)
	}
	if report.name != todayOutstandingReport || len(report.params) != 1 || report.params[0] != "" {
		t.Fatalf("report=%q params=%v", report.name, report.params)
	}
	if len(rows) != 1 || rows[0].AccountNumber != "1001" || rows[0].LoanStartDate.String() != "2024-06-01" || rows[0].Branch != "001" || rows[0].PrincipalOutstanding.Format(2) != "1234.50" || rows[0].PrincipalDue.Format(2) != "100.25" || rows[0].InterestDue.Format(2) != "20.00" || rows[0].PenaltyDue.Format(2) != "5.50" || rows[0].CollectabilityBI != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestSourceAcceptsExactPeriodStartAlias(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	date, _ := loan.ParseDate("2026-09-14", location)
	rows, err := NewSource(&reportStub{body: []byte("date_params|loan_account_no|periode_mulai|loan_outstanding|bi_collectability|principal_arrears|interest_arrears|penalty_arrears\n2026-09-14|1001|03/04/2025|10|1|0|0|0\n")}, location).FetchCurrentLoanPositions(context.Background(), date)
	if err != nil || len(rows) != 1 || rows[0].LoanStartDate.String() != "2025-04-03" {
		t.Fatalf("rows=%+v error=%v", rows, err)
	}
}

func TestSourceRejectsIncompleteOrConflictingDataset(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	date, _ := loan.ParseDate("2026-09-14", location)
	for _, test := range []struct{ name, body string }{
		{"missing column", "Date Params|Loan Account No\n2026-09-14|1\n"},
		{"missing penalty column", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears\n2026-09-14|1|2024-06-01|10|1|0|0\n"},
		{"missing loan start column", "Date Params|Loan Account No|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-14|1|10|1|0|0|0\n"},
		{"empty loan start date", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-14|1||10|1|0|0|0\n"},
		{"malformed loan start date", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-14|1|bad|10|1|0|0|0\n"},
		{"compact loan start date not accepted", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-14|1|20251013|10|1|0|0|0\n"},
		{"future loan start date", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-14|1|2026-09-15|10|1|0|0|0\n"},
		{"wrong date", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-13|1|2024-06-01|10|1|0|0|0\n"},
		{"malformed money", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-14|1|2024-06-01|bad|1|0|0|0\n"},
		{"negative penalty arrears", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-14|1|2024-06-01|10|1|0|0|-1\n"},
		{"conflicting duplicate penalty", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n2026-09-14|1|2024-06-01|10|1|0|0|0\n2026-09-14|1|2024-06-01|10|1|0|0|1\n"},
		{"empty", "Date Params|Loan Account No|start_date|Loan Outstanding|BI Collectability|Principal Arrears|Interest Arrears|Penalty Arrears\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := NewSource(&reportStub{body: []byte(test.body)}, location)
			_, err := source.FetchCurrentLoanPositions(context.Background(), date)
			if err == nil {
				t.Fatal("expected error")
			}
			if test.name == "missing penalty column" && !strings.Contains(err.Error(), `missing column "penalty arrears"`) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
