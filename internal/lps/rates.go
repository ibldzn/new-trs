package lps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const dailyRateURL = "https://apps.lps.go.id/LPSRate/ListHarian"

type RateEntry struct {
	Date time.Time
	BPR  float64
}

type HTTPRateProvider struct {
	http *http.Client
	url  string
}

func NewHTTPRateProvider(httpClient *http.Client) *HTTPRateProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPRateProvider{http: httpClient, url: dailyRateURL}
}

func (provider *HTTPRateProvider) Rates(ctx context.Context, asOf time.Time) ([]RateEntry, error) {
	form := url.Values{}
	form.Set("sort", `[{"selector":"startDate","desc":true}]`)
	form.Set("filter", fmt.Sprintf(`["startDate","<=","%s"]`, asOf.Format("2006-01-02")))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.url, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:149.0) Gecko/20100101 Firefox/149.0")
	request.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.Header.Set("Referer", "https://apps.lps.go.id/lpsrate/harian")

	response, err := provider.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("LPS rate request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("LPS rate request failed: %s", response.Status)
	}

	var payload struct {
		Data []struct {
			StartDate string  `json:"startDate"`
			RateBPR   float64 `json:"rateBPR"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode LPS rate response: %w", err)
	}
	entries := make([]RateEntry, 0, len(payload.Data))
	for index, row := range payload.Data {
		startDate := strings.TrimSpace(row.StartDate)
		if startDate == "" {
			continue
		}
		date, err := parseLPSRateDate(startDate)
		if err != nil {
			return nil, fmt.Errorf("row %d: invalid startDate", index+1)
		}
		entries = append(entries, RateEntry{Date: date, BPR: row.RateBPR})
	}
	if len(entries) == 0 {
		return nil, errors.New("no LPS rate rows found from LPS website")
	}
	return entries, nil
}

func parseLPSRateDate(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("date is empty")
	}
	for _, layout := range []string{"2006-01-02T15:04:05", time.RFC3339, "2006-01-02", "20060102"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC), nil
		}
	}
	return time.Time{}, errors.New("invalid date format")
}
