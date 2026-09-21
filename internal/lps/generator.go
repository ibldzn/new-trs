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
	Rates(context.Context, time.Time) ([]RateEntry, error)
}

type Generator struct {
	reports  ReportGateway
	cifs     CIFGateway
	debtors  DebtorTypeRepository
	rates    RateProvider
	location *time.Location
}

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
	rates, err := generator.rates.Rates(ctx, time.Now())
	if err != nil {
		return Result{}, fmt.Errorf("fetch LPS rates: %w", err)
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
	standingBody, err := generator.reports.DownloadNamedReport(ctx, "Standing Order Report csv", "ALL", "", "", "", "", "", "")
	if err != nil {
		return Result{}, fmt.Errorf("download Standing Order source: %w", err)
	}
	loanBody, err := generator.reports.DownloadMaintenanceReport(ctx, "cbrloan.csv", cbrPath)
	if err != nil {
		return Result{}, fmt.Errorf("download DK source: %w", err)
	}
	detailBody, err := generator.downloadLoanDetails(ctx, dailyPath)
	if err != nil {
		return Result{}, err
	}

	customers, seenCIF, err := parseCustomers(customerBody)
	if err != nil {
		return Result{}, fmt.Errorf("parse DN source: %w", err)
	}
	dnRows, err := generator.buildDN(ctx, customers)
	if err != nil {
		return Result{}, err
	}
	dsnRows, dsnCIF, err := buildDSN(savingsBody, depositBody, balanceBody, standingBody, input.ReportingDate, rates)
	if err != nil {
		return Result{}, err
	}
	dkRows, dkCIF, err := buildDK(loanBody, detailBody)
	if err != nil {
		return Result{}, err
	}
	for cif := range dkCIF {
		dsnCIF[cif] = struct{}{}
	}
	additionalDNRows, err := generator.completeMissingDN(ctx, seenCIF, dsnCIF)
	if err != nil {
		return Result{}, err
	}

	type fileContent struct {
		count int
		body  string
	}
	dnBody := rowsBody(dnRows)
	dnCount := strings.Count(dnBody, "\n")
	if len(additionalDNRows) != 0 {
		dnBody += strings.Join(additionalDNRows, "\n")
		dnCount += len(additionalDNRows)
	}
	dsnBody := rowsBody(dsnRows)
	dkBody := rowsBody(dkRows)
	result := Result{
		Filename: fmt.Sprintf("LPS_%s_%s.zip", input.ParticipantCode, input.ReportingDate),
		DNRows:   dnCount, DSNRows: strings.Count(dsnBody, "\n"), DKRows: strings.Count(dkBody, "\n"),
	}
	files := map[string]fileContent{
		fmt.Sprintf("DN_%s_%s_%s_%s.txt", input.ParticipantCode, input.ReportingDate, input.Period, input.Version):  {count: result.DNRows, body: dnBody},
		fmt.Sprintf("DSN_%s_%s_%s_%s.txt", input.ParticipantCode, input.ReportingDate, input.Period, input.Version): {count: result.DSNRows, body: dsnBody},
		fmt.Sprintf("DK_%s_%s_%s_%s.txt", input.ParticipantCode, input.ReportingDate, input.Period, input.Version):  {count: result.DKRows, body: dkBody},
		fmt.Sprintf("DSJ_%s_%s_%s_%s.txt", input.ParticipantCode, input.ReportingDate, input.Period, input.Version): {},
	}
	archive := zip.NewWriter(output)
	for _, name := range sortedKeys(files) {
		file, err := archive.Create(name)
		if err != nil {
			_ = archive.Close()
			return Result{}, fmt.Errorf("create LPS file: %w", err)
		}
		content := files[name]
		if _, err := file.Write(buildFileContent(input, content.count, content.body)); err != nil {
			_ = archive.Close()
			return Result{}, err
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
	var combined strings.Builder
	var headerSet map[string]struct{}
	for number := 1; number <= 8; number++ {
		name := fmt.Sprintf("DetailOutstandingRekeningPinjaman_%03d.csv", number)
		part, err := generator.reports.DownloadMaintenanceReport(ctx, name, path)
		if err != nil {
			return nil, fmt.Errorf("download loan detail part %03d: %w", number, err)
		}
		reader := newCSVReader(part)
		header, err := reader.Read()
		if err != nil {
			return nil, fmt.Errorf("read loan detail part %03d header: %w", number, err)
		}
		if number == 1 {
			combined.WriteString(strings.Join(header, "|"))
			combined.WriteByte('\n')
			headerSet = make(map[string]struct{}, len(header))
			for _, value := range header {
				headerSet[value] = struct{}{}
			}
		} else {
			if len(header) != len(headerSet) {
				return nil, fmt.Errorf("loan detail part %03d has inconsistent header", number)
			}
			for _, value := range header {
				if _, ok := headerSet[value]; !ok {
					return nil, fmt.Errorf("loan detail part %03d has inconsistent header", number)
				}
			}
		}
		if newline := bytes.IndexByte(part, '\n'); newline >= 0 {
			combined.Write(part[newline+1:])
		}
	}
	return []byte(combined.String()), nil
}

var dnColumns = []string{
	"cif_no", "cif_alternate_no", "customer_type", "customer_name", "idtype", "identity_number",
	"mother_maiden_name", "birth_date", "tax_id", "management_name", "management_identity", "address1",
	"citydati2", "phone_no", "debtor_type",
}

type customer struct {
	CIF, AlternateCIF, Type, Name, IdentityType, IdentityNumber, MotherName, BirthDate, TaxID string
	ManagementName, ManagementIdentity, Address, Dati2, Phone, DebtorType                     string
}

func parseCustomers(body []byte) ([]customer, map[string]struct{}, error) {
	table, err := parseTable(body)
	if err != nil {
		return nil, nil, err
	}
	if err := table.require(dnColumns...); err != nil {
		return nil, nil, err
	}
	indexes := make(map[string]int, len(dnColumns))
	for _, column := range dnColumns {
		indexes[column] = table.index(column)
	}
	rows := make([]customer, 0, len(table.rows))
	seen := make(map[string]struct{}, len(table.rows))
	for _, row := range table.rows {
		cif := strings.TrimSpace(valueAt(row, indexes["cif_no"]))
		if cif == "" {
			continue
		}
		if _, duplicate := seen[cif]; duplicate {
			continue
		}
		seen[cif] = struct{}{}
		rows = append(rows, customer{
			CIF: cif, AlternateCIF: valueAt(row, indexes["cif_alternate_no"]), Type: valueAt(row, indexes["customer_type"]),
			Name: valueAt(row, indexes["customer_name"]), IdentityType: valueAt(row, indexes["idtype"]),
			IdentityNumber: valueAt(row, indexes["identity_number"]), MotherName: valueAt(row, indexes["mother_maiden_name"]),
			BirthDate: valueAt(row, indexes["birth_date"]), TaxID: valueAt(row, indexes["tax_id"]),
			ManagementName: valueAt(row, indexes["management_name"]), ManagementIdentity: valueAt(row, indexes["management_identity"]),
			Address: valueAt(row, indexes["address1"]), Dati2: valueAt(row, indexes["citydati2"]),
			Phone: valueAt(row, indexes["phone_no"]), DebtorType: valueAt(row, indexes["debtor_type"]),
		})
	}
	return rows, seen, nil
}

func (generator *Generator) buildDN(ctx context.Context, customers []customer) ([]string, error) {
	rows := make([]string, 0, len(customers))
	for _, value := range customers {
		individual := strings.TrimSpace(value.Type) == "Perorangan"
		debtorType := "9002"
		if !individual {
			debtorType = strings.TrimSpace(value.DebtorType)
			if !isValidGolonganDebitur(debtorType) {
				fallback, err := generator.debtors.DebtorTypeByAlternateCIF(ctx, value.AlternateCIF)
				if err != nil || fallback == "" || fallback == "0002" {
					debtorType = "4599"
				} else {
					debtorType = fallback
				}
			}
		}
		managementIdentity := cleanAlamat(value.ManagementIdentity)
		if !individual && managementIdentity == "" {
			managementIdentity = strings.Repeat("0", 16)
		}
		dati2 := normalizeDati2(value.Dati2)
		identityNumber := strings.TrimSpace(value.IdentityNumber)
		if individual && identityNumber == "" {
			identityNumber = strings.Repeat("0", 16)
		}
		identityNumber = strings.ReplaceAll(identityNumber, " ", "")
		if managementDebtorTypes[debtorType] {
			identityNumber = ""
			if managementIdentity == "" {
				managementIdentity = strings.Repeat("0", 16)
			}
		}
		mother := cleanAlamat(strings.TrimSpace(value.MotherName))
		if individual && mother == "" {
			mother = "IBU KANDUNG"
		}
		identityType := strings.TrimSpace(value.IdentityType)
		if !slices.Contains([]string{"KTP", "PAS", "KTS"}, identityType) {
			identityType = "LN"
		}
		if !individual {
			identityType = ""
		}
		birthDate := strings.ReplaceAll(value.BirthDate, "-", "")
		if !individual {
			birthDate = ""
		}
		managementName := strings.TrimSpace(value.ManagementName)
		if !individual && managementName == "" {
			managementName = "BAPAK"
		}
		rows = append(rows, strings.Join([]string{
			"D", value.CIF, cleanAlamat(value.Name), identityType, identityNumber, mother, birthDate,
			valueIf(individual, value.TaxID, ""), managementName, valueIf(individual, "", "LN"), managementIdentity,
			cleanAlamat(value.Address), valueIf(dati2 == "", "0000", dati2), valueIf(individual, "WNI", ""),
			cleanAlamat(value.Phone), "1", "N", "20", debtorType,
		}, "|"))
	}
	return rows, nil
}

func (generator *Generator) completeMissingDN(ctx context.Context, seen, needed map[string]struct{}) ([]string, error) {
	missing := make([]string, 0)
	for cif := range needed {
		if _, ok := seen[cif]; !ok {
			seen[cif] = struct{}{}
			missing = append(missing, cif)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	type result struct {
		cif  string
		line string
		err  error
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
				line := ""
				if err == nil {
					line, err = generator.buildLiveDN(ctx, data)
				}
				results <- result{cif: cif, line: line, err: err}
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
	rows := make([]string, 0, len(missing))
	var firstError error
	for result := range results {
		if result.err != nil {
			if firstError == nil {
				firstError = fmt.Errorf("complete missing CIF %s: %w", result.cif, result.err)
			}
			continue
		}
		rows = append(rows, result.line)
	}
	if firstError != nil {
		return nil, firstError
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return rows, nil
}

func (generator *Generator) buildLiveDN(ctx context.Context, data fincloud.CIFData) (string, error) {
	individual := stringValue(data["jenisnasabah"]) == "Perorangan"
	identityType := ""
	if individual {
		identityType = stringValue(data["jenisidentitas"])
		if !slices.Contains([]string{"KTP", "PAS", "KTS"}, identityType) {
			identityType = "LN"
		}
	}
	debtorType := "9002"
	if !individual {
		debtorType = stringValue(data["datauntuksid_golongandebitur"])
		if !isValidGolonganDebitur(debtorType) {
			fallback, err := generator.debtors.DebtorTypeByAlternateCIF(ctx, stringValue(data["noalt"]))
			if err != nil || fallback == "" || fallback == "0002" {
				debtorType = "4599"
			} else {
				debtorType = fallback
			}
		}
	}
	identityNumber := stringValue(data["perorangan_noktp"])
	if individual && identityNumber == "" {
		identityNumber = strings.Repeat("0", 16)
	}
	mother := cleanAlamat(stringValue(data["perorangan_namaibukandung"]))
	if individual && mother == "" {
		mother = "IBU KANDUNG"
	}
	birthDate := strings.SplitN(objectString(data["dataktp_tgllahir"], "date"), " ", 2)[0]
	birthDate = strings.ReplaceAll(birthDate, "-", "")
	if !individual {
		birthDate = ""
	}
	managementName, managementIdentity := "", ""
	if !individual {
		if management, ok := firstObject(data["datapengurusperusahaan"]); ok {
			managementName = cleanAlamat(stringValue(management["nama"]))
			managementIdentity = cleanAlamat(stringValue(management["noktp"]))
			if managementIdentity == "" {
				managementIdentity = strings.Repeat("0", 16)
			}
		} else {
			managementName = "BAPAK"
		}
	}
	if managementDebtorTypes[debtorType] {
		identityNumber = ""
		if managementIdentity == "" {
			managementIdentity = strings.Repeat("0", 16)
		}
	}
	dati2 := normalizeDati2(stringValue(data["datauntuksid_dati2debitur"]))
	return strings.Join([]string{
		"D", stringValue(data["id"]), stringValue(data["namanasabah"]), identityType, identityNumber, mother, birthDate,
		valueIf(individual, stringValue(data["profilresiko_identitasnasabah"]), ""), managementName,
		valueIf(individual, "", "LN"), managementIdentity, cleanAlamat(stringValue(data["dataalamat_ktp_alamat1"])),
		dati2, valueIf(individual, "WNI", ""), cleanAlamat(stringValue(data["dataalamat_rumah_nohp"])),
		"1", "N", "20", debtorType,
	}, "|"), nil
}

var managementDebtorTypes = map[string]bool{"8139": true, "0070": true, "2090": true, "7174": true, "4120": true, "4599": true}

func normalizeDati2(value string) string {
	if len(value) == 3 {
		value = "0" + value
	}
	if !isValidDati2(value) {
		return "0000"
	}
	return value
}

type table struct {
	header []string
	rows   [][]string
}

func parseTable(body []byte) (table, error) {
	reader := newCSVReader(body)
	header, err := reader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return table{}, errors.New("empty CSV file")
		}
		return table{}, err
	}
	result := table{header: header}
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return table{}, err
		}
		result.rows = append(result.rows, row)
	}
	return result, nil
}

func newCSVReader(body []byte) *csv.Reader {
	reader := csv.NewReader(bytes.NewReader(body))
	reader.Comma = '|'
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true
	return reader
}

func (value table) index(name string) int {
	for index, header := range value.header {
		if strings.TrimSpace(header) == name {
			return index
		}
	}
	return -1
}

func (value table) require(columns ...string) error {
	for _, column := range columns {
		if value.index(column) == -1 {
			return errors.New("missing column in CSV: " + column)
		}
	}
	return nil
}

func transposeTable(body []byte, keyColumn string) (map[string]map[string]string, error) {
	table, err := parseTable(body)
	if err != nil {
		return nil, err
	}
	keyIndex := table.index(keyColumn)
	if keyIndex == -1 {
		return nil, errors.New("missing column in CSV: " + keyColumn)
	}
	result := make(map[string]map[string]string, len(table.rows))
	for _, row := range table.rows {
		key := valueAt(row, keyIndex)
		if _, ok := result[key]; !ok {
			result[key] = make(map[string]string)
		}
		for index, name := range table.header {
			result[key][name] = valueAt(row, index)
		}
	}
	return result, nil
}

func valueAt(row []string, index int) string {
	if index < 0 || index >= len(row) {
		return ""
	}
	return row[index]
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func objectString(value any, key string) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return stringValue(object[key])
}

func firstObject(value any) (map[string]any, bool) {
	values, ok := value.([]any)
	if !ok || len(values) == 0 {
		return nil, false
	}
	object, ok := values[0].(map[string]any)
	if !ok {
		object = map[string]any{}
	}
	return object, true
}

func rowsBody(rows []string) string {
	if len(rows) == 0 {
		return ""
	}
	return strings.Join(rows, "\n") + "\n"
}

func buildFileContent(input Input, count int, body string) []byte {
	header := fmt.Sprintf("H|%s|%s|%s|%s|%d", input.ParticipantCode, input.ReportingDate, input.Period, input.Version, count)
	if body == "" {
		return []byte(header + "\n")
	}
	return []byte(header + "\n" + body)
}

func valueIf(condition bool, ifTrue, ifFalse string) string {
	if condition {
		return ifTrue
	}
	return ifFalse
}

var (
	disallowedAddress = regexp.MustCompile(`[^0-9A-Za-z.,_'\/()& -]`)
	repeatedSpaces    = regexp.MustCompile(` +`)
)

func cleanAlamat(value string) string {
	value = disallowedAddress.ReplaceAllString(value, "")
	value = repeatedSpaces.ReplaceAllString(value, " ")
	return strings.TrimSpace(value)
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
