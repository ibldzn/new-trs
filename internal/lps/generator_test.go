package lps

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/loan"
)

type reportGatewayFake struct {
	files      map[string][]byte
	errors     map[string]error
	namedBody  []byte
	namedError error
	calls      []string
}

func (fake *reportGatewayFake) DownloadMaintenanceReport(_ context.Context, file, path string) ([]byte, error) {
	fake.calls = append(fake.calls, file)
	key := path + "/" + file
	if err := fake.errors[key]; err != nil {
		return nil, err
	}
	if body, ok := fake.files[key]; ok {
		return body, nil
	}
	return nil, loan.ErrNotFound
}

func (fake *reportGatewayFake) DownloadNamedReport(context.Context, string, ...string) ([]byte, error) {
	if fake.namedError != nil {
		return nil, fake.namedError
	}
	return fake.namedBody, nil
}

type cifGatewayFake map[string]fincloud.CIFData

func (fake cifGatewayFake) GetCIF(_ context.Context, cif string) (fincloud.CIFData, error) {
	value, ok := fake[cif]
	if !ok {
		return nil, loan.ErrNotFound
	}
	return value, nil
}

type debtorFake struct{}

func (debtorFake) DebtorTypeByAlternateCIF(_ context.Context, cif string) (string, error) {
	if cif == "ALT2" {
		return "7777", nil
	}
	return "", errors.New("MSO lookup failed")
}

type rateFake struct {
	entries []RateEntry
	calls   int
	asOf    time.Time
}

func (fake *rateFake) Rates(_ context.Context, asOf time.Time) ([]RateEntry, error) {
	fake.calls++
	fake.asOf = asOf
	return fake.entries, nil
}

func TestGeneratorMatchesOracleGoldenFiles(t *testing.T) {
	location := time.FixedZone("Jakarta", 7*60*60)
	cbr := "/app/report/cbr/20260914/"
	daily := "/app/report/daily/20260914/"
	reports := &reportGatewayFake{
		files: map[string][]byte{
			cbr + "cbrcustomer.csv":                            fixture(t, "cbrcustomer.csv"),
			cbr + "cbrsavings.csv":                             fixture(t, "cbrsavings.csv"),
			cbr + "cbrtimedeposit.csv":                         fixture(t, "cbrtimedeposit.csv"),
			daily + "Time Deposit Account Balance Details.csv": fixture(t, "time_deposit_balances.csv"),
			cbr + "cbrloan.csv":                                fixture(t, "cbrloan.csv"),
			daily + "DetailOutstandingRekeningPinjaman.csv":    fixture(t, "detail_outstanding.csv"),
		},
		namedBody: fixture(t, "standing_order.csv"),
	}
	cifs := cifGatewayFake{
		"C4": cifFixture(t, "live_cif_individual.json"),
		"C5": cifFixture(t, "live_cif_company.json"),
	}
	rates := &rateFake{entries: []RateEntry{
		{Date: mustRateDate(t, "2026-09-14"), BPR: 6.25},
		{Date: mustRateDate(t, "2026-01-01"), BPR: 6.5},
		{Date: mustRateDate(t, "2026-02-01"), BPR: 4.5},
	}}
	generator, err := NewGenerator(reports, cifs, debtorFake{}, rates, location)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var output bytes.Buffer
	result, err := generator.Generate(context.Background(), Input{
		ParticipantCode: "31300082", ReportingDate: "20260914", Period: "01", Version: "1",
	}, &output)
	if err != nil {
		t.Fatal(err)
	}
	if result != (Result{Filename: "LPS_31300082_20260914.zip", DNRows: 5, DSNRows: 6, DKRows: 2}) {
		t.Fatalf("result = %+v", result)
	}
	if rates.calls != 1 || rates.asOf.Before(started) || rates.asOf.After(time.Now()) {
		t.Fatalf("rate calls=%d asOf=%v", rates.calls, rates.asOf)
	}

	contents := unzip(t, output.Bytes())
	wantNames := []string{
		"DK_31300082_20260914_01_1.txt", "DN_31300082_20260914_01_1.txt",
		"DSJ_31300082_20260914_01_1.txt", "DSN_31300082_20260914_01_1.txt",
	}
	gotNames := make([]string, 0, len(contents))
	for name := range contents {
		gotNames = append(gotNames, name)
	}
	slices.Sort(gotNames)
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("archive names = %v", gotNames)
	}
	for name, body := range contents {
		if bytes.Contains(body, []byte{'\r'}) {
			t.Fatalf("%s contains CR", name)
		}
	}
	if got, want := string(contents[wantNames[0]]), string(fixture(t, "expected_DK.txt")); got != want {
		t.Fatalf("DK mismatch\ngot:  %q\nwant: %q", got, want)
	}
	if got, want := string(contents[wantNames[2]]), string(fixture(t, "expected_DSJ.txt")); got != want {
		t.Fatalf("DSJ mismatch\ngot:  %q\nwant: %q", got, want)
	}
	if got, want := string(contents[wantNames[3]]), string(fixture(t, "expected_DSN.txt")); got != want {
		t.Fatalf("DSN mismatch\ngot:  %q\nwant: %q", got, want)
	}
	assertDNGolden(t, contents[wantNames[1]], fixture(t, "expected_DN.txt"))
}

func TestOracleSchemaValidation(t *testing.T) {
	t.Run("DN complete header", func(t *testing.T) {
		body := bytes.Replace(fixture(t, "cbrcustomer.csv"), []byte("|debtor_type"), nil, 1)
		if _, _, err := parseCustomers(body); err == nil || !strings.Contains(err.Error(), "debtor_type") {
			t.Fatalf("error = %v", err)
		}
	})

	validSavings := fixture(t, "cbrsavings.csv")
	validDeposits := fixture(t, "cbrtimedeposit.csv")
	balances := fixture(t, "time_deposit_balances.csv")
	standing := fixture(t, "standing_order.csv")
	t.Run("missing DSN column", func(t *testing.T) {
		body := bytes.Replace(validSavings, []byte("|end_date"), nil, 1)
		if _, _, err := buildDSN(body, validDeposits, balances, standing, "20260914", nil); err == nil || !strings.Contains(err.Error(), "end_date") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("DSN column index mismatch", func(t *testing.T) {
		body := bytes.Replace(validDeposits,
			[]byte("cif_no|acc_no|product|start_date"), []byte("cif_no|acc_no|start_date|product"), 1)
		if _, _, err := buildDSN(validSavings, body, balances, standing, "20260914", nil); err == nil || !strings.Contains(err.Error(), "column index mismatch") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestOracleNumericParsing(t *testing.T) {
	for _, test := range []struct {
		input string
		want  float64
	}{{"1,000", 1000}, {"1,000.99", 1000.99}, {"bad", 0}} {
		if got := stringToFloat(test.input); got != test.want {
			t.Errorf("stringToFloat(%q) = %v, want %v", test.input, got, test.want)
		}
	}
	for _, test := range []struct {
		input string
		want  float64
	}{{"0.045", 4.5}, {"4,5%", 4.5}, {"4.5", 4.5}} {
		got, err := parsePercentage(test.input)
		if err != nil || got != test.want {
			t.Errorf("parsePercentage(%q) = %v, %v; want %v", test.input, got, err, test.want)
		}
	}
}

func TestStandingOrderDownloadIsRequired(t *testing.T) {
	reports := completeReportFake(t)
	reports.namedError = errors.New("standing order unavailable")
	generator, _ := NewGenerator(reports, cifGatewayFake{}, debtorFake{}, parityRates(), time.UTC)
	_, err := generator.Generate(context.Background(), Input{
		ParticipantCode: "31300082", ReportingDate: "20260914", Period: "01", Version: "1",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Standing Order") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoanDetailFallbackRequiresExactlyEightParts(t *testing.T) {
	path := "/app/report/daily/20260914"
	files := make(map[string][]byte)
	for number := 1; number <= 8; number++ {
		name := "DetailOutstandingRekeningPinjaman_00" + string(rune('0'+number)) + ".csv"
		files[path+"/"+name] = []byte("no_rekening|tunggakan_bunga\nL" + string(rune('0'+number)) + "|" + string(rune('0'+number)) + "\n")
	}
	reports := &reportGatewayFake{files: files, errors: map[string]error{path + "/DetailOutstandingRekeningPinjaman.csv": errors.New("monolithic failed")}}
	generator := &Generator{reports: reports}
	body, err := generator.downloadLoanDetails(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), "\n") != 9 || slices.Contains(reports.calls, "DetailOutstandingRekeningPinjaman_009.csv") {
		t.Fatalf("body=%q calls=%v", body, reports.calls)
	}
	delete(reports.files, path+"/DetailOutstandingRekeningPinjaman_008.csv")
	reports.calls = nil
	if _, err := generator.downloadLoanDetails(context.Background(), path); err == nil {
		t.Fatal("expected missing part 008 to fail")
	}
}

func TestHTTPRateProviderMatchesOracleContract(t *testing.T) {
	var requestForm url.Values
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		requestForm, _ = url.ParseQuery(string(body))
		if request.Method != http.MethodPost || request.Header.Get("X-Requested-With") != "XMLHttpRequest" ||
			request.Header.Get("Referer") != "https://apps.lps.go.id/lpsrate/harian" ||
			request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("request method=%s headers=%v", request.Method, request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
			Body: io.NopCloser(bytes.NewReader(fixture(t, "lps_rate_response.json"))), Request: request,
		}, nil
	})}
	provider := NewHTTPRateProvider(client)
	provider.url = "https://example.test/LPSRate/ListHarian"
	entries, err := provider.Rates(context.Background(), time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if requestForm.Get("sort") != `[{"selector":"startDate","desc":true}]` ||
		requestForm.Get("filter") != `["startDate","<=","2026-09-19"]` {
		t.Fatalf("form = %v", requestForm)
	}
	if len(entries) != 3 || entries[0].Date.Format("2006-01-02") != "2026-09-14" || entries[0].BPR != 6.25 {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestMissingExactRateUsesZero(t *testing.T) {
	savings := []byte("cif_no|acc_no|product|start_date|interest_rate|nominal|blocked_nominal|end_date\nC1|S1|101|old|4|100|0|old\n")
	deposits := []byte("cif_no|acc_no|product|start_date|interest_rate|nominal|blocked_nominal|end_date\nC1|D1|201|2020-01-01|4|100|0|2021-01-01\n")
	rows, _, err := buildDSN(savings, deposits, []byte("Account No|Accrued Interest\n"), []byte("Destination Account Number|End Date\n"), "20260914", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rows[0], "|0.00|1|") || !strings.Contains(rows[1], "|0.00|2.B|") {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestDNBaseRowsRetainTrailingLF(t *testing.T) {
	if got := rowsBody([]string{"D|base"}); got != "D|base\n" {
		t.Fatalf("body = %q", got)
	}
}

func assertDNGolden(t *testing.T, got, golden []byte) {
	t.Helper()
	if bytes.HasSuffix(got, []byte{'\n'}) {
		t.Fatalf("DN with live-CIF additions must not end in LF: %q", got)
	}
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(strings.TrimSuffix(string(golden), "\n"), "\n")
	if len(gotLines) != len(wantLines) {
		t.Fatalf("DN lines=%d, want %d: %q", len(gotLines), len(wantLines), got)
	}
	if !slices.Equal(gotLines[:4], wantLines[:4]) {
		t.Fatalf("deterministic DN prefix\ngot:  %#v\nwant: %#v", gotLines[:4], wantLines[:4])
	}
	slices.Sort(gotLines[4:])
	slices.Sort(wantLines[4:])
	if !slices.Equal(gotLines, wantLines) {
		t.Fatalf("DN records\ngot:  %#v\nwant: %#v", gotLines, wantLines)
	}
}

func completeReportFake(t *testing.T) *reportGatewayFake {
	t.Helper()
	cbr := "/app/report/cbr/20260914/"
	daily := "/app/report/daily/20260914/"
	return &reportGatewayFake{files: map[string][]byte{
		cbr + "cbrcustomer.csv":                            fixture(t, "cbrcustomer.csv"),
		cbr + "cbrsavings.csv":                             fixture(t, "cbrsavings.csv"),
		cbr + "cbrtimedeposit.csv":                         fixture(t, "cbrtimedeposit.csv"),
		daily + "Time Deposit Account Balance Details.csv": fixture(t, "time_deposit_balances.csv"),
		cbr + "cbrloan.csv":                                fixture(t, "cbrloan.csv"),
		daily + "DetailOutstandingRekeningPinjaman.csv":    fixture(t, "detail_outstanding.csv"),
	}, namedBody: fixture(t, "standing_order.csv")}
}

func parityRates() *rateFake {
	return &rateFake{entries: []RateEntry{
		{Date: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), BPR: 6.25},
		{Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), BPR: 6.5},
		{Date: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), BPR: 4.5},
	}}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/parity/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func cifFixture(t *testing.T, name string) fincloud.CIFData {
	t.Helper()
	var data fincloud.CIFData
	if err := json.Unmarshal(fixture(t, name), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func mustRateDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := parseLPSRateDate(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func unzip(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	contents := make(map[string][]byte, len(archive.File))
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		contents[file.Name], err = io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	return contents
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
