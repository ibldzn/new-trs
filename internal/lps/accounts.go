package lps

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var dsnColumns = []string{
	"cif_no", "acc_no", "product", "start_date", "interest_rate", "nominal", "blocked_nominal", "end_date",
}

func buildDSN(savingsBody, depositBody, balanceBody, standingBody []byte, reportingDate string, rates []RateEntry) ([]string, map[string]struct{}, error) {
	savings, err := parseTable(savingsBody)
	if err != nil {
		return nil, nil, fmt.Errorf("parse savings: %w", err)
	}
	deposits, err := parseTable(depositBody)
	if err != nil {
		return nil, nil, fmt.Errorf("parse deposits: %w", err)
	}
	for _, column := range dsnColumns {
		savingsIndex := savings.index(column)
		if savingsIndex == -1 {
			return nil, nil, errors.New("missing column in savings CSV: " + column)
		}
		depositIndex := deposits.index(column)
		if depositIndex == -1 {
			return nil, nil, errors.New("missing column in time deposit CSV: " + column)
		}
		if savingsIndex != depositIndex {
			return nil, nil, errors.New("column index mismatch between savings and time deposit CSV for column: " + column)
		}
	}

	balances, err := transposeTable(balanceBody, "Account No")
	if err != nil {
		return nil, nil, fmt.Errorf("parse deposit balances: %w", err)
	}
	standing, err := transposeTable(standingBody, "Destination Account Number")
	if err != nil {
		return nil, nil, fmt.Errorf("parse Standing Order source: %w", err)
	}

	indexes := make(map[string]int, len(dsnColumns))
	for _, column := range dsnColumns {
		indexes[column] = savings.index(column)
	}
	rateByDate := make(map[string]float64, len(rates))
	for _, entry := range rates {
		rateByDate[entry.Date.Format("2006-01-02")] = entry.BPR
	}
	reportingRateKey := reportingDate
	if parsed, err := parseLPSRateDate(reportingDate); err == nil {
		reportingRateKey = parsed.Format("2006-01-02")
	}

	rows := make([]string, 0, len(savings.rows)+len(deposits.rows))
	cifs := make(map[string]struct{})
	for _, row := range savings.rows {
		cif := strings.TrimSpace(valueAt(row, indexes["cif_no"]))
		if cif == "" {
			continue
		}
		cifs[cif] = struct{}{}
		account := strings.TrimSpace(valueAt(row, indexes["acc_no"]))
		if account == "" {
			continue
		}

		fundStatus := "S"
		for _, prefix := range []string{"103", "114", "115", "116", "117", "118"} {
			if strings.HasPrefix(valueAt(row, indexes["product"]), prefix) {
				fundStatus = "B"
				break
			}
		}
		endDate := reportingDate
		if fundStatus == "B" {
			endDate = standing[account]["End Date"]
			if endDate == "" {
				endDate = reportingDate
			}
		}
		blocked := stringToFloat(valueAt(row, indexes["blocked_nominal"]))
		blockReason := ""
		if blocked > 0 {
			blockReason = "99"
		}
		interestRateRaw := valueAt(row, indexes["interest_rate"])
		interestRate, err := parsePercentage(interestRateRaw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid interest rate %q for account %s: %w", interestRateRaw, valueAt(row, indexes["acc_no"]), err)
		}
		nominal := stringToFloat(valueAt(row, indexes["nominal"]))
		rows = append(rows, strings.Join([]string{
			"D", "R", "", cif, "TAB", valueAt(row, indexes["acc_no"]), fundStatus,
			strings.ReplaceAll(reportingDate, "-", ""), "1", fmt.Sprintf("%.2f", interestRate), "0",
			fmt.Sprintf("%.2f", rateByDate[reportingRateKey]), "1", fmt.Sprintf("%d", int(nominal)),
			fmt.Sprintf("%d", int(blocked)), blockReason, "0", "", strings.ReplaceAll(endDate, "-", ""),
		}, "|"))
	}

	for _, row := range deposits.rows {
		cif := strings.TrimSpace(valueAt(row, indexes["cif_no"]))
		if cif == "" {
			continue
		}
		cifs[cif] = struct{}{}
		account := strings.TrimSpace(valueAt(row, indexes["acc_no"]))
		if account == "" {
			continue
		}
		startDate := strings.TrimSpace(valueAt(row, indexes["start_date"]))
		lpsRate := rateByDate[startDate]
		interestRateRaw := valueAt(row, indexes["interest_rate"])
		interestRate, err := parsePercentage(interestRateRaw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid interest rate %q for account %s: %w", interestRateRaw, valueAt(row, indexes["acc_no"]), err)
		}
		nominal := stringToFloat(valueAt(row, indexes["nominal"]))
		accrued := stringToFloat(balances[account]["Accrued Interest"])
		lastAccrual := ""
		if accrued > 0 {
			lastAccrual = reportingDate
		}
		category := "1"
		if interestRate > lpsRate {
			category = "2.B"
		}
		rows = append(rows, strings.Join([]string{
			"D", "R", "", cif, "DEP", account, "B", strings.ReplaceAll(startDate, "-", ""), "1",
			fmt.Sprintf("%.2f", interestRate), "0", fmt.Sprintf("%.2f", lpsRate), category,
			fmt.Sprintf("%d", int(nominal)), "0", "", fmt.Sprintf("%d", int(accrued)), lastAccrual,
			strings.ReplaceAll(valueAt(row, indexes["end_date"]), "-", ""),
		}, "|"))
	}
	return rows, cifs, nil
}

func buildDK(body, detailBody []byte) ([]string, map[string]struct{}, error) {
	loans, err := parseTable(body)
	if err != nil {
		return nil, nil, fmt.Errorf("parse loans: %w", err)
	}
	columns := []string{"cifno", "loan_no", "collectibility", "credit_limit_effective", "outstanding", "principal_arrears", "start_date", "mature_date"}
	if err := loans.require(columns...); err != nil {
		return nil, nil, err
	}
	details, err := transposeTable(detailBody, "no_rekening")
	if err != nil {
		return nil, nil, fmt.Errorf("parse loan details: %w", err)
	}
	indexes := make(map[string]int, len(columns))
	for _, column := range columns {
		indexes[column] = loans.index(column)
	}
	rows := make([]string, 0, len(loans.rows))
	cifs := make(map[string]struct{})
	for _, row := range loans.rows {
		cif := strings.TrimSpace(valueAt(row, indexes["cifno"]))
		if cif == "" {
			continue
		}
		cifs[cif] = struct{}{}
		account := strings.TrimSpace(valueAt(row, indexes["loan_no"]))
		if account == "" {
			continue
		}
		plafond := stringToFloat(valueAt(row, indexes["credit_limit_effective"]))
		outstanding := stringToFloat(valueAt(row, indexes["outstanding"]))
		principalArrears := stringToFloat(valueAt(row, indexes["principal_arrears"]))
		interestArrears := stringToFloat(details[account]["tunggakan_bunga"])
		rows = append(rows, strings.Join([]string{
			"D", cif, account, "03", valueAt(row, indexes["collectibility"]), fmt.Sprintf("%d", int(plafond)),
			fmt.Sprintf("%d", int(outstanding)), fmt.Sprintf("%d", int(principalArrears)), fmt.Sprintf("%d", int(interestArrears)),
			"300", strings.ReplaceAll(valueAt(row, indexes["start_date"]), "-", ""),
			strings.ReplaceAll(valueAt(row, indexes["mature_date"]), "-", ""), "4",
		}, "|"))
	}
	return rows, cifs, nil
}

func stringToFloat(value string) float64 {
	parsed, err := strconv.ParseFloat(strings.ReplaceAll(value, ",", ""), 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parsePercentage(value string) (float64, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return 0, errors.New("empty percentage")
	}
	hasPercent := strings.HasSuffix(raw, "%")
	if hasPercent {
		raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	}
	raw = strings.ReplaceAll(raw, ",", ".")
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	if !hasPercent && parsed > 0 && parsed < 1 {
		parsed *= 100
	}
	return parsed, nil
}
