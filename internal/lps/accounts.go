package lps

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

func buildDSN(
	savingsBody, depositBody []byte,
	depositBalances map[string]loan.Money,
	standing map[string]string,
	reportingRate loan.Money,
	reportingDate time.Time,
	rateAt func(time.Time) (loan.Money, error),
	location *time.Location,
) ([]string, map[string]struct{}, error) {
	rows := make([]string, 0)
	cifs := make(map[string]struct{})
	savings, err := parseTable(savingsBody)
	if err != nil {
		return nil, nil, fmt.Errorf("parse savings: %w", err)
	}
	for _, row := range savings.rows {
		cif := field(row, "cif_no", "cifno")
		account := field(row, "account_number", "account_no", "savings_account_no", "rekening")
		product := field(row, "product_id", "product")
		startDate, err := requiredDate(field(row, "start_date", "open_date"), location)
		if err != nil || cif == "" || account == "" || product == "" {
			return nil, nil, fmt.Errorf("savings row missing mandatory identity/date")
		}
		interestRate, err := requiredMoneyField(row, "interest_rate", "rate")
		if err != nil {
			return nil, nil, fmt.Errorf("savings %s interest rate: %w", account, err)
		}
		nominal, err := requiredMoneyField(row, "nominal", "balance", "current_balance")
		if err != nil {
			return nil, nil, fmt.Errorf("savings %s nominal: %w", account, err)
		}
		blocked, err := optionalMoneyField(row, "blocked_nominal", "blocked_amount")
		if err != nil {
			return nil, nil, fmt.Errorf("savings %s blocked nominal: %w", account, err)
		}
		owners := field(row, "owners", "owner_count", "number_of_owners")
		if owners == "" {
			owners = "1"
		}
		fundStatus := "S"
		for _, prefix := range []string{"103", "114", "115", "116", "117", "118"} {
			if strings.HasPrefix(product, prefix) {
				fundStatus = "B"
				break
			}
		}
		endDate := ""
		if fundStatus == "B" {
			raw := standing[account]
			if raw == "" {
				endDate = reportingDate.Format("20060102")
			} else if endDate, err = requiredDate(raw, location); err != nil {
				return nil, nil, fmt.Errorf("savings %s Standing Order date: %w", account, err)
			}
		} else if raw := field(row, "end_date", "maturity_date"); raw != "" {
			endDate, err = requiredDate(raw, location)
			if err != nil {
				return nil, nil, fmt.Errorf("savings %s end date: %w", account, err)
			}
		}
		blockReason := ""
		if blocked.IsPositive() {
			blockReason = "99"
		}
		rows = append(rows, strings.Join([]string{
			"D", "R", owners, cif, "TAB", account, fundStatus, startDate, "1", interestRate.Format(2), "0", reportingRate.Format(2), "1",
			nominal.Format(2), blocked.Format(2), blockReason, "0", "", endDate,
		}, "|"))
		cifs[cif] = struct{}{}
	}

	deposits, err := parseTable(depositBody)
	if err != nil {
		return nil, nil, fmt.Errorf("parse deposits: %w", err)
	}
	for _, row := range deposits.rows {
		cif := field(row, "cif_no", "cifno")
		account := field(row, "account_number", "account_no", "deposit_account", "rekening")
		startRaw := field(row, "start_date", "open_date")
		start, err := parseDate(startRaw, location)
		if err != nil || cif == "" || account == "" {
			return nil, nil, fmt.Errorf("deposit row missing mandatory identity/date")
		}
		startDate := start.Format("20060102")
		endDate, err := requiredDate(field(row, "end_date", "maturity_date"), location)
		if err != nil {
			return nil, nil, fmt.Errorf("deposit %s maturity date: %w", account, err)
		}
		interestRate, err := requiredMoneyField(row, "interest_rate", "rate")
		if err != nil {
			return nil, nil, fmt.Errorf("deposit %s interest rate: %w", account, err)
		}
		nominal, err := requiredMoneyField(row, "nominal", "balance", "current_balance")
		if err != nil {
			return nil, nil, fmt.Errorf("deposit %s nominal: %w", account, err)
		}
		lpsRate, err := rateAt(start)
		if err != nil {
			return nil, nil, fmt.Errorf("deposit %s LPS rate: %w", account, err)
		}
		accrued, ok := depositBalances[account]
		if !ok {
			return nil, nil, fmt.Errorf("deposit %s missing accrued interest", account)
		}
		owners := field(row, "owners", "owner_count", "number_of_owners")
		if owners == "" {
			owners = "1"
		}
		category := "1"
		if interestRate.Cmp(lpsRate) > 0 {
			category = "2.B"
		}
		lastAccrual := ""
		if accrued.IsPositive() {
			lastAccrual = reportingDate.Format("20060102")
		}
		rows = append(rows, strings.Join([]string{
			"D", "R", owners, cif, "DEP", account, "B", startDate, "1", interestRate.Format(2), "0", lpsRate.Format(2), category,
			nominal.Format(2), "0", "", accrued.Format(2), lastAccrual, endDate,
		}, "|"))
		cifs[cif] = struct{}{}
	}
	return rows, cifs, nil
}

func buildDK(body []byte, interestArrears map[string]loan.Money, location *time.Location) ([]string, map[string]struct{}, error) {
	table, err := parseTable(body)
	if err != nil {
		return nil, nil, fmt.Errorf("parse loans: %w", err)
	}
	rows := make([]string, 0, len(table.rows))
	cifs := make(map[string]struct{})
	for _, row := range table.rows {
		cif := field(row, "cifno", "cif_no")
		account := field(row, "loan_no", "loan_account_no", "account_number", "no_rekening")
		collectability := field(row, "collectibility", "collectability", "bi_collectability")
		value, err := strconv.Atoi(collectability)
		if err != nil || value < 1 || value > 5 || cif == "" || account == "" {
			return nil, nil, fmt.Errorf("loan row has invalid identity or collectability")
		}
		plafond, err := requiredMoneyField(row, "credit_limit_effective", "credit_limit", "plafond")
		if err != nil {
			return nil, nil, fmt.Errorf("loan %s plafond: %w", account, err)
		}
		outstanding, err := requiredMoneyField(row, "outstanding", "loan_outstanding")
		if err != nil {
			return nil, nil, fmt.Errorf("loan %s outstanding: %w", account, err)
		}
		principalDue, err := requiredMoneyField(row, "principal_arrears", "tunggakan_pokok")
		if err != nil {
			return nil, nil, fmt.Errorf("loan %s principal arrears: %w", account, err)
		}
		interestDue, ok := interestArrears[account]
		if !ok {
			return nil, nil, fmt.Errorf("loan %s missing interest arrears", account)
		}
		startDate, err := requiredDate(field(row, "start_date"), location)
		if err != nil {
			return nil, nil, fmt.Errorf("loan %s start date: %w", account, err)
		}
		maturityDate, err := requiredDate(field(row, "mature_date", "maturity_date", "end_date"), location)
		if err != nil {
			return nil, nil, fmt.Errorf("loan %s maturity date: %w", account, err)
		}
		rows = append(rows, strings.Join([]string{
			"D", cif, account, "03", collectability, plafond.Format(2), outstanding.Format(2), principalDue.Format(2), interestDue.Format(2), "300", startDate, maturityDate, "4",
		}, "|"))
		cifs[cif] = struct{}{}
	}
	return rows, cifs, nil
}

func requiredMoneyField(row map[string]string, names ...string) (loan.Money, error) {
	raw := field(row, names...)
	if raw == "" {
		return loan.Money{}, fmt.Errorf("required monetary field is empty")
	}
	return parseMoney(raw)
}

func optionalMoneyField(row map[string]string, names ...string) (loan.Money, error) {
	raw := field(row, names...)
	if raw == "" {
		return loan.Money{}, nil
	}
	return parseMoney(raw)
}
