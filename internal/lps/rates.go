package lps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

const dailyRateURL = "https://apps.lps.go.id/LPSRate/ListHarian"

type HTTPRateProvider struct {
	http *http.Client
	url  string
}

func NewHTTPRateProvider(httpClient *http.Client) *HTTPRateProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &HTTPRateProvider{http: httpClient, url: dailyRateURL}
}

func (provider *HTTPRateProvider) RateAt(ctx context.Context, date time.Time) (loan.Money, error) {
	form := url.Values{"tanggal": {date.Format("2006-01-02")}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.url, strings.NewReader(form.Encode()))
	if err != nil {
		return loan.Money{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := provider.http.Do(request)
	if err != nil {
		return loan.Money{}, fmt.Errorf("LPS rate request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return loan.Money{}, fmt.Errorf("LPS rate request returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return loan.Money{}, err
	}
	if len(body) > 2<<20 {
		return loan.Money{}, fmt.Errorf("LPS rate response exceeds size limit")
	}
	var payload any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return loan.Money{}, fmt.Errorf("decode LPS rate response: %w", err)
	}
	points := make([]ratePoint, 0)
	if err := collectRatePoints(payload, &points, date.Location()); err != nil {
		return loan.Money{}, err
	}
	var selected *ratePoint
	for index := range points {
		point := &points[index]
		if point.date.After(date) {
			continue
		}
		if selected == nil || point.date.After(selected.date) {
			selected = point
		}
	}
	if selected == nil {
		return loan.Money{}, errors.New("LPS rate response contains no rate at or before requested date")
	}
	return selected.rate, nil
}

type ratePoint struct {
	date time.Time
	rate loan.Money
}

func collectRatePoints(value any, points *[]ratePoint, location *time.Location) error {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if err := collectRatePoints(item, points, location); err != nil {
				return err
			}
		}
	case map[string]any:
		var dateRaw, rateRaw string
		for key, raw := range typed {
			name := canonical(key)
			switch name {
			case "tanggal", "date", "tanggal_berlaku", "berlaku_mulai", "start_date":
				dateRaw = fmt.Sprint(raw)
			case "rate_bpr", "bpr", "suku_bunga_bpr", "tingkat_bunga_penjaminan_bpr", "rate":
				rateRaw = fmt.Sprint(raw)
			}
		}
		if dateRaw != "" && rateRaw != "" {
			date, dateErr := parseDate(dateRaw, location)
			rate, rateErr := parseMoney(rateRaw)
			if dateErr != nil || rateErr != nil {
				return fmt.Errorf("malformed LPS rate row")
			}
			*points = append(*points, ratePoint{date: date, rate: rate})
		}
		for _, nested := range typed {
			if err := collectRatePoints(nested, points, location); err != nil {
				return err
			}
		}
	}
	return nil
}
