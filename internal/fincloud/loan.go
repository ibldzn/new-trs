package fincloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

var tenorPattern = regexp.MustCompile(`[0-9]+`)

type scalar struct {
	value   string
	present bool
}

type scheduleDTO struct {
	Date          string `json:"tanggal"`
	InstallmentNo int64  `json:"angsuranke"`
}

func (value *scalar) UnmarshalJSON(raw []byte) error {
	value.present = true
	if string(raw) == "null" {
		value.value = ""
		return nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		return json.Unmarshal(raw, &value.value)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return fmt.Errorf("expected string or number")
	}
	value.value = number.String()
	return nil
}

type loanDTO struct {
	ID             string        `json:"id"`
	NoAlt          string        `json:"noalt"`
	CIF            string        `json:"nocif"`
	CIFNumber      string        `json:"cifno"`
	CustomerName   string        `json:"namanasabah"`
	Branch         string        `json:"rec_dibuat_lokasi"`
	Product        string        `json:"produkid"`
	PlafondLimit   scalar        `json:"plafondlimit"`
	Tenor          scalar        `json:"jangkawaktu"`
	FlatRate       scalar        `json:"bungaflat"`
	ReferenceRate  scalar        `json:"produk_sukubunga"`
	Collectability int           `json:"kolekbi"`
	PrincipalDue   scalar        `json:"tunggakanpokok"`
	InterestDue    scalar        `json:"tunggakanbunga"`
	PenaltyDue     scalar        `json:"dendatunggakan"`
	Status         string        `json:"statusrekening"`
	CloseDate      string        `json:"tgltutup"`
	Schedule       []scheduleDTO `json:"jadwalangsuran"`
	Repayments     []struct {
		Date          string `json:"tglbayar"`
		Principal     scalar `json:"bayar_pokok"`
		Interest      scalar `json:"bayar_bunga"`
		Penalty       scalar `json:"bayar_denda"`
		EarlyPenalty  scalar `json:"bayar_dendapelunasan"`
		DWP           scalar `json:"nominaldwp"`
		Total         scalar `json:"totalbayar"`
		JournalNumber string `json:"nojurnal"`
	} `json:"historybayar"`
}

func (client *Client) ResolveLoan(ctx context.Context, account string, location *time.Location) (loan.ContractData, error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return loan.ContractData{}, fmt.Errorf("%w: account number is required", loan.ErrInvalidInput)
	}
	detail, err := client.GetLoan(ctx, account, location)
	if err == nil {
		return detail, nil
	}
	if !errors.Is(err, loan.ErrNotFound) {
		return loan.ContractData{}, err
	}
	primary, err := client.resolveAlternate(ctx, account)
	if err != nil {
		return loan.ContractData{}, err
	}
	detail, err = client.GetLoan(ctx, primary, location)
	if err != nil {
		return loan.ContractData{}, err
	}
	if detail.PrimaryAccount != primary {
		return loan.ContractData{}, fmt.Errorf("%w: Fincloud returned primary %q after resolving %q", loan.ErrInvariant, detail.PrimaryAccount, primary)
	}
	return detail, nil
}

func (client *Client) GetLoan(ctx context.Context, account string, location *time.Location) (loan.ContractData, error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return loan.ContractData{}, fmt.Errorf("%w: account number is required", loan.ErrInvalidInput)
	}
	body, err := client.do(ctx, defaultJSONLimit, func(ctx context.Context, session string) (*http.Request, error) {
		return client.newRequest(ctx, http.MethodGet, "/pinjaman/inquiry/rekening/pinjaman", url.Values{"id": {account}}, nil)
	})
	if err != nil {
		return loan.ContractData{}, err
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result loanDTO `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return loan.ContractData{}, errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("decode loan detail: %w", err))
	}
	if envelope.Status != "ok" {
		return loan.ContractData{}, loan.ErrFincloudUnavailable
	}
	if strings.TrimSpace(envelope.Data.Result.ID) == "" {
		return loan.ContractData{}, loan.ErrNotFound
	}
	return mapLoan(envelope.Data.Result, location)
}

func (client *Client) resolveAlternate(ctx context.Context, alternate string) (string, error) {
	body, err := client.do(ctx, defaultJSONLimit, func(ctx context.Context, session string) (*http.Request, error) {
		query := url.Values{"cabang": {"ALL"}, "noalt": {alternate}, "pagesize": {"50"}}
		return client.newRequest(ctx, http.MethodGet, "/pinjaman/inquiry/rekening/cari", query, nil)
	})
	if err != nil {
		return "", err
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				ID    string `json:"id"`
				NoAlt string `json:"noalt"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("decode alternate search: %w", err))
	}
	if envelope.Status != "ok" {
		return "", loan.ErrFincloudUnavailable
	}
	candidates := make(map[string]struct{})
	for _, row := range envelope.Data.Result {
		primary := strings.TrimSpace(row.ID)
		returnedAlternate := strings.TrimSpace(row.NoAlt)
		if primary != "" && (returnedAlternate == "" || returnedAlternate == alternate) {
			candidates[primary] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return "", loan.ErrNotFound
	}
	if len(candidates) > 1 {
		return "", loan.ErrAmbiguousAccountResolution
	}
	for primary := range candidates {
		return primary, nil
	}
	return "", loan.ErrNotFound
}

func mapLoan(source loanDTO, location *time.Location) (loan.ContractData, error) {
	if location == nil {
		return loan.ContractData{}, fmt.Errorf("business timezone is required")
	}
	principal, err := requiredMoney("plafondlimit", source.PlafondLimit)
	if err != nil {
		return loan.ContractData{}, err
	}
	flatRate, err := requiredMoney("bungaflat", source.FlatRate)
	if err != nil {
		return loan.ContractData{}, err
	}
	referenceRate, err := optionalMoney("produk_sukubunga", source.ReferenceRate)
	if err != nil {
		return loan.ContractData{}, err
	}
	principalDue, err := optionalMoney("tunggakanpokok", source.PrincipalDue)
	if err != nil {
		return loan.ContractData{}, err
	}
	interestDue, err := optionalMoney("tunggakanbunga", source.InterestDue)
	if err != nil {
		return loan.ContractData{}, err
	}
	penaltyDue, err := optionalMoney("dendatunggakan", source.PenaltyDue)
	if err != nil {
		return loan.ContractData{}, err
	}
	tenorText := tenorPattern.FindString(strings.TrimSpace(source.Tenor.value))
	tenor, err := strconv.Atoi(tenorText)
	if !source.Tenor.present || err != nil || tenor <= 0 {
		return loan.ContractData{}, errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("invalid jangkawaktu"))
	}
	result := loan.ContractData{
		PrimaryAccount: strings.TrimSpace(source.ID), AlternateAccount: strings.TrimSpace(source.NoAlt), CIF: strings.TrimSpace(source.CIF),
		CustomerName: strings.TrimSpace(source.CustomerName), Branch: strings.TrimSpace(source.Branch), Product: strings.TrimSpace(source.Product),
		PlafondLimit: principal, TenorMonths: tenor, FlatRatePercent: flatRate, ReferenceRatePercent: referenceRate,
		CurrentCollectability: source.Collectability, CurrentPrincipalDue: principalDue, CurrentInterestDue: interestDue,
		PenaltyDue: penaltyDue, Status: strings.TrimSpace(source.Status), RawScheduleCount: len(source.Schedule),
	}
	if result.CIF == "" {
		result.CIF = strings.TrimSpace(source.CIFNumber)
	}
	if strings.TrimSpace(source.CloseDate) != "" {
		result.CloseDate, err = parseDate(source.CloseDate, location)
		if err != nil {
			return loan.ContractData{}, errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("invalid tgltutup: %w", err))
		}
	}
	result.ContractScheduleEvidence = make([]loan.ContractualInstallmentEvidence, len(source.Schedule))
	for index, row := range source.Schedule {
		result.ContractScheduleEvidence[index] = loan.ContractualInstallmentEvidence{Number: row.InstallmentNo, RawDueDate: row.Date}
	}
	result.Repayments = make([]loan.Repayment, 0, len(source.Repayments))
	for index, row := range source.Repayments {
		date, err := parseDate(row.Date, location)
		if err != nil {
			return loan.ContractData{}, errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("invalid repayment date: %w", err))
		}
		principal, err := requiredMoney("bayarpokok", row.Principal)
		if err != nil {
			return loan.ContractData{}, err
		}
		interest, err := requiredMoney("bayarbunga", row.Interest)
		if err != nil {
			return loan.ContractData{}, err
		}
		total, err := requiredMoney("totalbayar", row.Total)
		if err != nil {
			return loan.ContractData{}, err
		}
		penalty, err := optionalMoney("bayardenda", row.Penalty)
		if err != nil {
			return loan.ContractData{}, err
		}
		earlyPenalty, err := optionalMoney("bayardendapelunasan", row.EarlyPenalty)
		if err != nil {
			return loan.ContractData{}, err
		}
		dwp, err := optionalMoney("nominaldwp", row.DWP)
		if err != nil {
			return loan.ContractData{}, err
		}
		result.Repayments = append(result.Repayments, loan.Repayment{
			Date: date, PrincipalComponent: principal, InterestComponent: interest, PenaltyComponent: penalty,
			EarlyPenaltyComponent: earlyPenalty, DWPComponent: dwp, TotalPayment: total,
			JournalNumber: strings.TrimSpace(row.JournalNumber), SourceOrder: index,
		})
	}
	return result, nil
}

func requiredMoney(field string, value scalar) (loan.Money, error) {
	if !value.present || strings.TrimSpace(value.value) == "" {
		return loan.Money{}, errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("missing %s", field))
	}
	return parseMoney(field, value.value)
}

func optionalMoney(field string, value scalar) (loan.Money, error) {
	if !value.present || strings.TrimSpace(value.value) == "" {
		return loan.Money{}, nil
	}
	return parseMoney(field, value.value)
}

func parseMoney(field, raw string) (loan.Money, error) {
	normalized, err := NormalizeDecimal(raw)
	if err != nil {
		return loan.Money{}, errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("invalid %s: %w", field, err))
	}
	value, err := loan.ParseMoney(normalized)
	if err != nil {
		return loan.Money{}, errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("invalid %s: %w", field, err))
	}
	return value, nil
}

func parseDate(raw string, location *time.Location) (loan.Date, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) >= len(loan.DateLayout) {
		raw = raw[:len(loan.DateLayout)]
	}
	return loan.ParseDate(raw, location)
}

func NormalizeDecimal(raw string) (string, error) {
	value := strings.TrimSpace(strings.ReplaceAll(raw, "%", ""))
	if value == "" || value == "-" {
		return "", fmt.Errorf("empty decimal")
	}
	for _, character := range value {
		if (character < '0' || character > '9') && character != ',' && character != '.' && character != '-' && character != ' ' {
			return "", fmt.Errorf("unsupported character %q", character)
		}
	}
	value = strings.ReplaceAll(value, " ", "")
	if strings.Count(value, "-") > 1 || (strings.Contains(value, "-") && !strings.HasPrefix(value, "-")) {
		return "", fmt.Errorf("invalid minus sign")
	}
	comma := strings.LastIndex(value, ",")
	dot := strings.LastIndex(value, ".")
	switch {
	case comma >= 0 && dot < 0:
		value = strings.ReplaceAll(value, ",", ".")
	case comma >= 0 && dot >= 0 && comma > dot:
		value = strings.ReplaceAll(value, ".", "")
		value = strings.ReplaceAll(value, ",", ".")
	case comma >= 0 && dot >= 0:
		value = strings.ReplaceAll(value, ",", "")
	case strings.Count(value, ".") > 1:
		parts := strings.Split(value, ".")
		first := strings.TrimPrefix(parts[0], "-")
		grouped := len(first) >= 1 && len(first) <= 3
		for _, part := range parts[1:] {
			if len(part) != 3 {
				grouped = false
			}
		}
		if grouped {
			value = strings.ReplaceAll(value, ".", "")
		} else {
			last := parts[len(parts)-1]
			value = strings.Join(parts[:len(parts)-1], "") + "." + last
		}
	case dot >= 0:
		parts := strings.Split(value, ".")
		if len(parts) == 2 && len(parts[0]) <= 3 && len(parts[1]) == 3 {
			value = parts[0] + parts[1]
		}
	}
	return value, nil
}

type CIFData map[string]any

func (client *Client) GetCIF(ctx context.Context, cif string) (CIFData, error) {
	body, err := client.do(ctx, defaultJSONLimit, func(ctx context.Context, session string) (*http.Request, error) {
		return client.newRequest(ctx, http.MethodGet, "/cif/inquiry/cif/cif", url.Values{"nocif": {strings.TrimSpace(cif)}}, nil)
	})
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			Result CIFData `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Status != "ok" || envelope.Data.Result == nil {
		return nil, errors.Join(loan.ErrFincloudUnavailable, fmt.Errorf("invalid CIF response"))
	}
	return envelope.Data.Result, nil
}

func (client *Client) DownloadMaintenanceReport(ctx context.Context, file, path string) ([]byte, error) {
	return client.do(ctx, client.maxReportSize, func(ctx context.Context, session string) (*http.Request, error) {
		query := url.Values{"file": {file}, "path": {path}, "sessionId": {session}}
		return client.newRequest(ctx, http.MethodGet, "/system/downloaderlaporan/download.php", query, nil)
	})
}

func (client *Client) DownloadNamedReport(ctx context.Context, name string, params ...string) ([]byte, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode report parameters: %w", err)
	}
	return client.do(ctx, client.maxReportSize, func(ctx context.Context, session string) (*http.Request, error) {
		query := url.Values{"nm": {name}, "type": {"csv"}, "p": {string(encoded)}}
		return client.newRequest(ctx, http.MethodGet, "/system/laporanUmum/data/lap", query, nil)
	})
}
