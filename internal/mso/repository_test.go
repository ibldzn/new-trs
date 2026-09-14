package mso

import (
	"errors"
	"testing"

	"github.com/ibldzn/trs/internal/loan"
)

func TestParseCollectabilityBI(t *testing.T) {
	for input, want := range map[string]int{
		"1": 1, "L": 1,
		"2": 2, "DPK": 2,
		"3": 3, "KL": 3,
		"4": 4, "D": 4,
		"5": 5, "M": 5,
	} {
		got, err := parseCollectabilityBI(input)
		if err != nil || got != want {
			t.Errorf("parseCollectabilityBI(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-", "6", "unknown"} {
		if _, err := parseCollectabilityBI(input); !errors.Is(err, loan.ErrHistoricalEvidence) {
			t.Errorf("parseCollectabilityBI(%q) error = %v", input, err)
		}
	}
}
