package lps

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ibldzn/trs/internal/fincloud"
	"github.com/ibldzn/trs/internal/loan"
)

type ReportGateway interface {
	DownloadMaintenanceReport(context.Context, string, string) ([]byte, error)
	DownloadNamedReport(context.Context, string, ...string) ([]byte, error)
}

type CIFGateway interface {
	GetCIF(context.Context, string) (fincloud.CIFData, error)
}

type DebtorTypeRepository interface {
	DebtorTypeByAlternateCIF(context.Context, string) (string, error)
}

type RateProvider interface {
	RateAt(context.Context, time.Time) (loan.Money, error)
}

type Generator struct {
	reports  ReportGateway
	cifs     CIFGateway
	debtors  DebtorTypeRepository
	rates    RateProvider
	location *time.Location
}

const maxCombinedLoanDetailBytes = 100 << 20

type Input struct {
	ParticipantCode string
	ReportingDate   string
	Period          string
	Version         string
}

type Result struct {
	Filename string
	DNRows   int
	DSNRows  int
	DKRows   int
	DSJRows  int
}

func NewGenerator(reports ReportGateway, cifs CIFGateway, debtors DebtorTypeRepository, rates RateProvider, location *time.Location) (*Generator, error) {
	if reports == nil || cifs == nil || debtors == nil || rates == nil || location == nil {
		return nil, fmt.Errorf("LPS generator dependencies are required")
	}
	return &Generator{reports: reports, cifs: cifs, debtors: debtors, rates: rates, location: location}, nil
}

func (generator *Generator) Generate(ctx context.Context, input Input, output io.Writer) (Result, error) {
	date, err := generator.validate(input)
	if err != nil {
		return Result{}, err
	}
	datePath := date.Format("20060102")
	cbrPath := "/app/report/cbr/" + datePath
	dailyPath := "/app/report/daily/" + datePath

	customerBody, err := generator.reports.DownloadMaintenanceReport(ctx, "cbrcustomer.csv", cbrPath)
	if err != nil {
		return Result{}, fmt.Errorf("download DN source: %w", err)
	}
	savingsBody, err := generator.reports.DownloadMaintenanceReport(ctx, "cbrsavings.csv", cbrPath)
	if err != nil {
		return Result{}, fmt.Errorf("download savings source: %w", err)
	}
	depositBody, err := generator.reports.DownloadMaintenanceReport(ctx, "cbrtimedeposit.csv", cbrPath)
	if err != nil {
		return Result{}, fmt.Errorf("download deposit source: %w", err)
	}
	balanceBody, err := generator.reports.DownloadMaintenanceReport(ctx, "Time Deposit Account Balance Details.csv", dailyPath)
	if err != nil {
		return Result{}, fmt.Errorf("download deposit balance source: %w", err)
	}
	loanBody, err := generator.reports.DownloadMaintenanceReport(ctx, "cbrloan.csv", cbrPath)
	if err != nil {
		return Result{}, fmt.Errorf("download DK source: %w", err)
	}
	detailBody, err := generator.downloadLoanDetails(ctx, dailyPath)
	if err != nil {
		return Result{}, err
	}
	standingBody, _ := generator.reports.DownloadNamedReport(ctx, "Standing Order Report csv", "ALL", "", "", "", "", "", "")

	customers, err := parseCustomers(customerBody)
	if err != nil {
		return Result{}, fmt.Errorf("parse DN source: %w", err)
	}
	standing, err := parseOptionalAccountDates(standingBody)
	if err != nil {
		return Result{}, fmt.Errorf("parse Standing Order source: %w", err)
	}
	balances, err := parseAccountMoney(balanceBody, []string{"account_number", "account_no", "rekening", "deposit_account"}, []string{"accrued_interest", "accrue_interest", "bunga_berjalan"})
	if err != nil {
		return Result{}, fmt.Errorf("parse deposit balances: %w", err)
	}
	interestArrears, err := parseAccountMoney(detailBody, []string{"loan_account_no", "loan_no", "account_number", "no_rekening"}, []string{"interest_arrears", "tunggakan_bunga"})
	if err != nil {
		return Result{}, fmt.Errorf("parse loan details: %w", err)
	}

	rateCache := make(map[string]loan.Money)
	rateAt := func(value time.Time) (loan.Money, error) {
		key := value.Format("20060102")
		if rate, ok := rateCache[key]; ok {
			return rate, nil
		}
		rate, err := generator.rates.RateAt(ctx, value)
		if err != nil {
			return loan.Money{}, err
		}
		if rate.IsNegative() || rate.IsZero() {
			return loan.Money{}, fmt.Errorf("LPS rate is missing or invalid for %s", key)
		}
		rateCache[key] = rate
		return rate, nil
	}
	reportingRate, err := rateAt(date)
	if err != nil {
		return Result{}, err
	}
	dsnRows, referencedCIF, err := buildDSN(savingsBody, depositBody, balances, standing, reportingRate, date, rateAt, generator.location)
	if err != nil {
		return Result{}, err
	}
	dkRows, loanCIF, err := buildDK(loanBody, interestArrears, generator.location)
	if err != nil {
		return Result{}, err
	}
	for cif := range loanCIF {
		referencedCIF[cif] = struct{}{}
	}
	if err := generator.completeMissingCustomers(ctx, customers, referencedCIF); err != nil {
		return Result{}, err
	}
	dnRows, err := generator.buildDN(ctx, customers)
	if err != nil {
		return Result{}, err
	}
	slices.Sort(dnRows)
	slices.Sort(dsnRows)
	slices.Sort(dkRows)
	result := Result{Filename: fmt.Sprintf("LPS_%s_%s.zip", input.ParticipantCode, input.ReportingDate), DNRows: len(dnRows), DSNRows: len(dsnRows), DKRows: len(dkRows)}
	files := map[string][]string{
		fmt.Sprintf("DN_%s_%s_%s_%s.txt", input.ParticipantCode, input.ReportingDate, input.Period, input.Version):  dnRows,
		fmt.Sprintf("DSN_%s_%s_%s_%s.txt", input.ParticipantCode, input.ReportingDate, input.Period, input.Version): dsnRows,
		fmt.Sprintf("DK_%s_%s_%s_%s.txt", input.ParticipantCode, input.ReportingDate, input.Period, input.Version):  dkRows,
		fmt.Sprintf("DSJ_%s_%s_%s_%s.txt", input.ParticipantCode, input.ReportingDate, input.Period, input.Version): {},
	}
	archive := zip.NewWriter(output)
	for _, name := range sortedKeys(files) {
		file, err := archive.Create(name)
		if err != nil {
			_ = archive.Close()
			return Result{}, fmt.Errorf("create LPS file: %w", err)
		}
		rows := files[name]
		if _, err := fmt.Fprintf(file, "H|%s|%s|%s|%s|%d\r\n", input.ParticipantCode, input.ReportingDate, input.Period, input.Version, len(rows)); err != nil {
			_ = archive.Close()
			return Result{}, err
		}
		for _, row := range rows {
			if _, err := io.WriteString(file, row+"\r\n"); err != nil {
				_ = archive.Close()
				return Result{}, err
			}
		}
	}
	if err := archive.Close(); err != nil {
		return Result{}, fmt.Errorf("finalize LPS archive: %w", err)
	}
	return result, nil
}

func (generator *Generator) validate(input Input) (time.Time, error) {
	if !digits(input.ParticipantCode, 8) || !digits(input.ReportingDate, 8) || !safeToken(input.Period) || !safeToken(input.Version) {
		return time.Time{}, fmt.Errorf("%w: invalid LPS input", loan.ErrInvalidInput)
	}
	date, err := time.ParseInLocation("20060102", input.ReportingDate, generator.location)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid reporting date", loan.ErrInvalidInput)
	}
	return date, nil
}

func (generator *Generator) downloadLoanDetails(ctx context.Context, path string) ([]byte, error) {
	body, err := generator.reports.DownloadMaintenanceReport(ctx, "DetailOutstandingRekeningPinjaman.csv", path)
	if err == nil {
		return body, nil
	}
	if !errors.Is(err, loan.ErrNotFound) {
		return nil, fmt.Errorf("download loan details: %w", err)
	}
	var combined bytes.Buffer
	var header string
	parts := 0
	for number := 1; number <= 999; number++ {
		name := fmt.Sprintf("DetailOutstandingRekeningPinjaman_%03d.csv", number)
		part, partErr := generator.reports.DownloadMaintenanceReport(ctx, name, path)
		if errors.Is(partErr, loan.ErrNotFound) && parts > 0 {
			break
		}
		if partErr != nil {
			return nil, fmt.Errorf("download loan detail part %03d: %w", number, partErr)
		}
		if combined.Len()+len(part) > maxCombinedLoanDetailBytes {
			return nil, fmt.Errorf("combined loan detail report exceeds size limit")
		}
		lines := strings.Split(strings.TrimPrefix(string(part), "\uFEFF"), "\n")
		if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
			return nil, fmt.Errorf("loan detail part %03d has no header", number)
		}
		currentHeader := strings.TrimRight(lines[0], "\r")
		if header == "" {
			header = currentHeader
			combined.WriteString(header + "\n")
		} else if currentHeader != header {
			return nil, fmt.Errorf("loan detail part %03d has inconsistent header", number)
		}
		for _, line := range lines[1:] {
			line = strings.TrimRight(line, "\r")
			if strings.TrimSpace(line) != "" {
				combined.WriteString(line + "\n")
			}
		}
		parts++
	}
	if parts == 0 {
		return nil, fmt.Errorf("mandatory loan detail report is unavailable")
	}
	return combined.Bytes(), nil
}

type customer struct {
	CIF, AlternateCIF, Type, Name, IdentityType, IdentityNumber, MotherName, BirthDate, TaxID string
	ManagementName, ManagementIdentity, Address, Dati2, Phone, DebtorType                     string
}

func parseCustomers(body []byte) (map[string]customer, error) {
	table, err := parseTable(body)
	if err != nil {
		return nil, err
	}
	if err := table.require("cif_no", "customer_type", "customer_name", "idtype", "identity_number", "birth_date", "address1", "citydati2", "debtor_type"); err != nil {
		return nil, err
	}
	customers := make(map[string]customer, len(table.rows))
	for _, row := range table.rows {
		value := customerFromRow(row)
		if value.CIF == "" {
			return nil, fmt.Errorf("customer row has empty CIF")
		}
		if existing, duplicate := customers[value.CIF]; duplicate && existing != value {
			return nil, fmt.Errorf("conflicting customer rows for CIF %s", value.CIF)
		}
		customers[value.CIF] = value
	}
	return customers, nil
}

func customerFromRow(row map[string]string) customer {
	return customer{
		CIF: field(row, "cif_no", "cifno"), AlternateCIF: field(row, "cif_alternate_no", "cif_alt_no"), Type: field(row, "customer_type"),
		Name: field(row, "customer_name", "name"), IdentityType: field(row, "idtype", "identity_type"), IdentityNumber: field(row, "identity_number"),
		MotherName: field(row, "mother_maiden_name"), BirthDate: field(row, "birth_date"), TaxID: field(row, "tax_id", "npwp"),
		ManagementName: field(row, "management_name"), ManagementIdentity: field(row, "management_identity"), Address: field(row, "address1", "address"),
		Dati2: field(row, "citydati2", "dati2"), Phone: field(row, "phone_no", "phone"), DebtorType: field(row, "debtor_type"),
	}
}

func (generator *Generator) completeMissingCustomers(ctx context.Context, customers map[string]customer, needed map[string]struct{}) error {
	missing := make([]string, 0)
	for cif := range needed {
		if cif != "" {
			if _, ok := customers[cif]; !ok {
				missing = append(missing, cif)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	type result struct {
		cif   string
		value customer
		err   error
	}
	work := make(chan string)
	results := make(chan result, len(missing))
	var wait sync.WaitGroup
	for range min(5, len(missing)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for cif := range work {
				data, err := generator.cifs.GetCIF(ctx, cif)
				row := make(map[string]string, len(data))
				for key, raw := range data {
					row[canonical(key)] = strings.TrimSpace(fmt.Sprint(raw))
				}
				value := customerFromRow(row)
				if value.CIF == "" {
					value.CIF = cif
				}
				results <- result{cif: cif, value: value, err: err}
			}
		}()
	}
	go func() {
		defer close(work)
		for _, cif := range missing {
			select {
			case work <- cif:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wait.Wait(); close(results) }()
	var firstError error
	for result := range results {
		if result.err != nil {
			if firstError == nil {
				firstError = fmt.Errorf("complete missing CIF %s: %w", result.cif, result.err)
			}
			continue
		}
		customers[result.cif] = result.value
	}
	if firstError != nil {
		return firstError
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (generator *Generator) buildDN(ctx context.Context, customers map[string]customer) ([]string, error) {
	rows := make([]string, 0, len(customers))
	for _, value := range customers {
		individual := strings.EqualFold(strings.TrimSpace(value.Type), "Perorangan")
		if strings.TrimSpace(value.CIF) == "" || strings.TrimSpace(value.Name) == "" || strings.TrimSpace(value.Type) == "" {
			return nil, fmt.Errorf("mandatory customer identity is missing for CIF %s", value.CIF)
		}
		debtorType := strings.TrimSpace(value.DebtorType)
		if individual {
			if !validDebtorType(debtorType) {
				debtorType = "9002"
			}
		} else if !validDebtorType(debtorType) || debtorType == "0002" {
			fallback, err := generator.debtors.DebtorTypeByAlternateCIF(ctx, value.AlternateCIF)
			if err != nil && !errors.Is(err, loan.ErrNotFound) {
				return nil, fmt.Errorf("resolve debtor type for CIF %s: %w", value.CIF, err)
			}
			debtorType = strings.TrimSpace(fallback)
			if !validDebtorType(debtorType) || debtorType == "0002" {
				debtorType = "4599"
			}
		}
		identityType := strings.ToUpper(strings.TrimSpace(value.IdentityType))
		identityNumber := clean(value.IdentityNumber)
		mother := clean(value.MotherName)
		managementName := clean(value.ManagementName)
		managementIdentity := clean(value.ManagementIdentity)
		managementIdentityType := ""
		citizenship := ""
		if individual {
			if identityType != "KTP" && identityType != "PAS" && identityType != "KTS" {
				identityType = "LN"
			}
			if identityNumber == "" {
				identityNumber = strings.Repeat("0", 16)
			}
			if mother == "" {
				mother = "IBU KANDUNG"
			}
			citizenship = "WNI"
		} else {
			identityType = ""
			managementIdentityType = "LN"
			if managementIdentity == "" {
				managementIdentity = strings.Repeat("0", 16)
			}
			if managementName == "" {
				managementName = "BAPAK"
			}
		}
		if managementDebtorTypes[debtorType] {
			identityNumber = ""
			if managementIdentity == "" {
				managementIdentity = strings.Repeat("0", 16)
			}
		}
		dati2 := normalizeDati2(value.Dati2)
		birthDate, err := optionalDate(value.BirthDate, generator.location)
		if err != nil {
			return nil, fmt.Errorf("invalid birth date for CIF %s", value.CIF)
		}
		taxID := ""
		if individual {
			taxID = clean(value.TaxID)
		}
		rows = append(rows, strings.Join([]string{
			"D", clean(value.CIF), clean(value.Name), identityType, identityNumber, mother, birthDate, taxID,
			managementName, managementIdentityType, managementIdentity, clean(value.Address), dati2, citizenship,
			clean(value.Phone), "1", "N", "20", debtorType,
		}, "|"))
	}
	return rows, nil
}

var managementDebtorTypes = map[string]bool{"8139": true, "0070": true, "2090": true, "7174": true, "4120": true, "4599": true}

// ponytail: numeric validation until owner supplies authoritative exhaustive LPS code sets.
func validDebtorType(value string) bool { return digits(value, 4) }
func normalizeDati2(value string) string {
	value = strings.TrimSpace(value)
	if digits(value, 3) {
		value = "0" + value
	}
	if !digits(value, 4) {
		return "0000"
	}
	return value
}

type table struct {
	headers map[string]struct{}
	rows    []map[string]string
}

func parseTable(body []byte) (table, error) {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	if len(bytes.TrimSpace(body)) == 0 {
		return table{}, fmt.Errorf("report is empty")
	}
	firstLine := body
	if index := bytes.IndexByte(body, '\n'); index >= 0 {
		firstLine = body[:index]
	}
	delimiter := ','
	if bytes.Count(firstLine, []byte("|")) > bytes.Count(firstLine, []byte(",")) {
		delimiter = '|'
	}
	reader := csv.NewReader(bytes.NewReader(body))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return table{}, err
	}
	result := table{headers: make(map[string]struct{}, len(headers)), rows: make([]map[string]string, 0)}
	canonicalHeaders := make([]string, len(headers))
	for index, header := range headers {
		canonicalHeaders[index] = canonical(header)
		result.headers[canonicalHeaders[index]] = struct{}{}
	}
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return table{}, err
		}
		row := make(map[string]string, len(headers))
		nonempty := false
		for index, header := range canonicalHeaders {
			if index < len(record) {
				row[header] = strings.TrimSpace(record[index])
				nonempty = nonempty || row[header] != ""
			}
		}
		if nonempty {
			result.rows = append(result.rows, row)
		}
	}
	return result, nil
}

func (value table) require(columns ...string) error {
	for _, column := range columns {
		if _, ok := value.headers[column]; !ok {
			return fmt.Errorf("required column %q is missing", column)
		}
	}
	return nil
}

func canonical(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var output strings.Builder
	underscore := false
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			output.WriteRune(character)
			underscore = false
		} else if !underscore && output.Len() > 0 {
			output.WriteByte('_')
			underscore = true
		}
	}
	return strings.Trim(output.String(), "_")
}

func field(row map[string]string, names ...string) string {
	for _, name := range names {
		if value, ok := row[name]; ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseAccountMoney(body []byte, accountColumns, valueColumns []string) (map[string]loan.Money, error) {
	table, err := parseTable(body)
	if err != nil {
		return nil, err
	}
	result := make(map[string]loan.Money, len(table.rows))
	for _, row := range table.rows {
		account := field(row, accountColumns...)
		raw := field(row, valueColumns...)
		if account == "" || raw == "" {
			return nil, fmt.Errorf("account or monetary value is missing")
		}
		value, err := parseMoney(raw)
		if err != nil {
			return nil, err
		}
		if existing, duplicate := result[account]; duplicate && existing.Cmp(value) != 0 {
			return nil, fmt.Errorf("conflicting monetary rows for account %s", account)
		}
		result[account] = value
	}
	return result, nil
}

func parseOptionalAccountDates(body []byte) (map[string]string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]string{}, nil
	}
	table, err := parseTable(body)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, row := range table.rows {
		account := field(row, "destination_account", "account_destination", "rekening_tujuan", "to_account")
		date := field(row, "end_date", "tanggal_akhir", "maturity_date")
		if account != "" && date != "" {
			result[account] = date
		}
	}
	return result, nil
}

func parseMoney(raw string) (loan.Money, error) {
	normalized, err := fincloud.NormalizeDecimal(raw)
	if err != nil {
		return loan.Money{}, err
	}
	return loan.ParseMoney(normalized)
}

func requiredDate(raw string, location *time.Location) (string, error) {
	value, err := parseDate(raw, location)
	if err != nil {
		return "", err
	}
	return value.Format("20060102"), nil
}

func optionalDate(raw string, location *time.Location) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	return requiredDate(raw, location)
}

func parseDate(raw string, location *time.Location) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 10 && raw[4] == '-' {
		raw = raw[:10]
	}
	for _, layout := range []string{"2006-01-02", "20060102", "02/01/2006"} {
		if value, err := time.ParseInLocation(layout, raw, location); err == nil {
			return value, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q", raw)
}

var repeatedSpace = regexp.MustCompile(`\s+`)

func clean(value string) string {
	var output strings.Builder
	for _, character := range strings.TrimSpace(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || strings.ContainsRune(" .,/&'-", character) {
			output.WriteRune(character)
		}
	}
	return repeatedSpace.ReplaceAllString(strings.TrimSpace(output.String()), " ")
}

func digits(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func safeToken(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
