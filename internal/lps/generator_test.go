package lps

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/loan"
)

type reportGatewayFake struct {
	files map[string][]byte
	err   error
}

func (fake *reportGatewayFake) DownloadMaintenanceReport(_ context.Context, file, path string) ([]byte, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	body, ok := fake.files[path+"/"+file]
	if !ok {
		return nil, loan.ErrNotFound
	}
	return body, nil
}
func (fake *reportGatewayFake) DownloadNamedReport(context.Context, string, ...string) ([]byte, error) {
	return []byte("destination_account|end_date\nS1|2026-12-31\n"), nil
}

type cifGatewayFake struct{}

func (cifGatewayFake) GetCIF(_ context.Context, cif string) (fincloud.CIFData, error) {
	return fincloud.CIFData{
		"cif_no": cif, "customer_type": "Perorangan", "customer_name": "Missing Customer", "idtype": "KTP",
		"identity_number": "", "mother_maiden_name": "", "birth_date": "1980-01-02", "address1": "Address", "citydati2": "123", "debtor_type": "9002",
	}, nil
}

type debtorFake struct{}

func (debtorFake) DebtorTypeByAlternateCIF(context.Context, string) (string, error) {
	return "", loan.ErrNotFound
}

type rateFake struct{}

func (rateFake) RateAt(context.Context, time.Time) (loan.Money, error) {
	return loan.MustMoney("6.5"), nil
}

func TestGeneratorBuildsFourFilesAndCompletesMissingCIF(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	cbr := "/app/report/cbr/20260914/"
	daily := "/app/report/daily/20260914/"
	reports := &reportGatewayFake{files: map[string][]byte{
		cbr + "cbrcustomer.csv":                            []byte("cif_no|cif_alternate_no|customer_type|customer_name|idtype|identity_number|mother_maiden_name|birth_date|tax_id|management_name|management_identity|address1|citydati2|phone_no|debtor_type\nC1|A1|Perorangan|Customer One|OTHER|||1980-01-01||||Jl. One|123|021|\n"),
		cbr + "cbrsavings.csv":                             []byte("cif_no|account_number|product_id|start_date|interest_rate|nominal|blocked_nominal\nC1|S1|103A|2026-01-01|4.5|1000|10\n"),
		cbr + "cbrtimedeposit.csv":                         []byte("cif_no|account_number|start_date|end_date|interest_rate|nominal\nC2|D1|2026-01-01|2026-12-31|7|2000\n"),
		daily + "Time Deposit Account Balance Details.csv": []byte("account_number|accrued_interest\nD1|25\n"),
		cbr + "cbrloan.csv":                                []byte("cifno|loan_no|collectibility|credit_limit_effective|outstanding|principal_arrears|start_date|mature_date\nC2|L1|3|5000|4000|100|2025-01-01|2027-01-01\n"),
		daily + "DetailOutstandingRekeningPinjaman.csv":    []byte("loan_account_no|interest_arrears\nL1|50\n"),
	}}
	generator, err := NewGenerator(reports, cifGatewayFake{}, debtorFake{}, rateFake{}, location)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	result, err := generator.Generate(context.Background(), Input{ParticipantCode: "31300082", ReportingDate: "20260914", Period: "01", Version: "1"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if result.DNRows != 2 || result.DSNRows != 2 || result.DKRows != 1 || result.DSJRows != 0 || result.Filename != "LPS_31300082_20260914.zip" {
		t.Fatalf("result = %+v", result)
	}
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 4 {
		t.Fatalf("files = %d", len(archive.File))
	}
	contents := make(map[string]string)
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(reader)
		_ = reader.Close()
		contents[file.Name] = string(body)
	}
	if !strings.Contains(contents["DN_31300082_20260914_01_1.txt"], "H|31300082|20260914|01|1|2") || !strings.Contains(contents["DN_31300082_20260914_01_1.txt"], "Missing Customer") {
		t.Fatalf("DN = %q", contents["DN_31300082_20260914_01_1.txt"])
	}
	if !strings.Contains(contents["DSN_31300082_20260914_01_1.txt"], "|2.B|") || !strings.Contains(contents["DK_31300082_20260914_01_1.txt"], "|L1|03|3|5000.00|4000.00|100.00|50.00|") {
		t.Fatalf("DSN=%q DK=%q", contents["DSN_31300082_20260914_01_1.txt"], contents["DK_31300082_20260914_01_1.txt"])
	}
	if contents["DSJ_31300082_20260914_01_1.txt"] != "H|31300082|20260914|01|1|0\r\n" {
		t.Fatalf("DSJ = %q", contents["DSJ_31300082_20260914_01_1.txt"])
	}
}

func TestGeneratorFailsWhenMandatoryReportUnavailable(t *testing.T) {
	generator, _ := NewGenerator(&reportGatewayFake{err: errors.New("upstream down")}, cifGatewayFake{}, debtorFake{}, rateFake{}, time.UTC)
	if _, err := generator.Generate(context.Background(), Input{ParticipantCode: "31300082", ReportingDate: "20260914", Period: "01", Version: "1"}, io.Discard); err == nil {
		t.Fatal("expected error")
	}
}

func TestHTTPRateProviderSelectsLatestRateAtOrBeforeDate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Fatalf("method = %s", request.Method)
		}
		body, _ := io.ReadAll(request.Body)
		values, _ := url.ParseQuery(string(body))
		if values.Get("tanggal") != "2026-09-14" {
			t.Fatalf("form = %v", values)
		}
		fmt.Fprint(writer, `{"data":[{"tanggal":"2026-09-01","rate_bpr":"6.25"},{"tanggal":"2026-09-15","rate_bpr":"9"},{"tanggal":"2026-09-10","rate_bpr":"6.50"}]}`)
	}))
	defer server.Close()
	provider := NewHTTPRateProvider(server.Client())
	provider.url = server.URL
	rate, err := provider.RateAt(context.Background(), time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC))
	if err != nil || rate.Format(2) != "6.50" {
		t.Fatalf("rate=%s error=%v", rate.Format(2), err)
	}
}
