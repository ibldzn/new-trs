package snapshot

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/loan"
)

const todayOutstandingReport = "Loan Outstanding Details Report Today"

type reportDownloader interface {
	DownloadNamedReport(context.Context, string, ...string) ([]byte, error)
}

type Source struct {
	reports  reportDownloader
	location *time.Location
}

func NewSource(reports reportDownloader, location *time.Location) *Source {
	return &Source{reports: reports, location: location}
}

func (source *Source) FetchCurrentLoanPositions(ctx context.Context, businessDate loan.Date) ([]loan.LoanPosition, error) {
	body, err := source.reports.DownloadNamedReport(ctx, todayOutstandingReport, "")
	if err != nil {
		return nil, err
	}
	return source.parse(body, businessDate)
}

func (source *Source) parse(body []byte, businessDate loan.Date) ([]loan.LoanPosition, error) {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	reader := csv.NewReader(bytes.NewReader(body))
	reader.Comma = '|'
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read current snapshot header: %w", err)
	}
	columns := make(map[string]int)
	for index, name := range header {
		columns[canonicalHeader(name)] = index
	}
	for _, required := range []string{"date params", "loan account no", "loan outstanding", "bi collectability", "principal arrears", "interest arrears"} {
		if _, ok := columns[required]; !ok {
			return nil, fmt.Errorf("current snapshot report missing column %q", required)
		}
	}
	rows := make([]loan.LoanPosition, 0)
	seen := make(map[string]loan.LoanPosition)
	for line := 2; ; line++ {
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("parse current snapshot line %d: %w", line, readErr)
		}
		if blankRecord(record) {
			continue
		}
		account := strings.TrimSpace(cell(record, columns, "loan account no"))
		if account == "" {
			return nil, fmt.Errorf("current snapshot line %d has empty account", line)
		}
		asOf, err := parseSourceDate(cell(record, columns, "date params"), source.location)
		if err != nil || !asOf.Equal(businessDate) {
			return nil, fmt.Errorf("current snapshot line %d has invalid business date", line)
		}
		principal, err := parseSourceMoney(cell(record, columns, "loan outstanding"))
		if err != nil {
			return nil, fmt.Errorf("current snapshot line %d loan outstanding: %w", line, err)
		}
		principalDue, err := parseSourceMoney(cell(record, columns, "principal arrears"))
		if err != nil {
			return nil, fmt.Errorf("current snapshot line %d principal arrears: %w", line, err)
		}
		interestDue, err := parseSourceMoney(cell(record, columns, "interest arrears"))
		if err != nil {
			return nil, fmt.Errorf("current snapshot line %d interest arrears: %w", line, err)
		}
		collectabilityText := strings.TrimSpace(cell(record, columns, "bi collectability"))
		collectability, parseErr := strconv.Atoi(collectabilityText)
		if parseErr != nil || collectability < 1 || collectability > 5 {
			return nil, fmt.Errorf("current snapshot line %d has invalid collectability", line)
		}
		if principal.IsNegative() || principalDue.IsNegative() || interestDue.IsNegative() || principalDue.Cmp(principal) > 0 {
			return nil, fmt.Errorf("current snapshot line %d has invalid balances", line)
		}
		branch := strings.TrimSpace(cell(record, columns, "branch code"))
		if before, _, found := strings.Cut(branch, "-"); found {
			branch = before
		}
		row := loan.LoanPosition{
			AsOf: asOf, AccountNumber: account, PrincipalOutstanding: principal, PrincipalDue: principalDue,
			InterestDue: interestDue, CollectabilityBI: collectability, Source: loan.SourceTodaySnapshot,
			Branch: strings.TrimSpace(branch), Product: strings.TrimSpace(cell(record, columns, "product id")),
			CIF: strings.TrimSpace(cell(record, columns, "cif no")), ContractNumber: strings.TrimSpace(cell(record, columns, "loan agreement no")),
		}
		if previous, duplicate := seen[account]; duplicate {
			if !samePosition(previous, row) {
				return nil, fmt.Errorf("current snapshot contains conflicting duplicate account %q", account)
			}
			continue
		}
		seen[account] = row
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("current snapshot report contains no data rows")
	}
	return rows, nil
}

func canonicalHeader(value string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", " ")), " ")
}

func cell(record []string, columns map[string]int, name string) string {
	index, ok := columns[name]
	if !ok || index >= len(record) {
		return ""
	}
	return record[index]
}

func blankRecord(record []string) bool {
	for _, value := range record {
		if strings.TrimSpace(value) != "" {
			return false
		}
	}
	return true
}

func parseSourceMoney(raw string) (loan.Money, error) {
	normalized, err := fincloud.NormalizeDecimal(raw)
	if err != nil {
		return loan.Money{}, err
	}
	return loan.ParseMoney(normalized)
}

func parseSourceDate(raw string, location *time.Location) (loan.Date, error) {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{"2006-01-02", "20060102", "02/01/2006"} {
		value, err := time.ParseInLocation(layout, raw, location)
		if err == nil {
			return loan.NewDate(value, location), nil
		}
	}
	return loan.Date{}, fmt.Errorf("invalid date %q", raw)
}

func samePosition(left, right loan.LoanPosition) bool {
	return left.AsOf.Equal(right.AsOf) && left.PrincipalOutstanding.Cmp(right.PrincipalOutstanding) == 0 &&
		left.PrincipalDue.Cmp(right.PrincipalDue) == 0 && left.InterestDue.Cmp(right.InterestDue) == 0 &&
		left.CollectabilityBI == right.CollectabilityBI && left.Branch == right.Branch && left.Product == right.Product && left.CIF == right.CIF && left.ContractNumber == right.ContractNumber
}
