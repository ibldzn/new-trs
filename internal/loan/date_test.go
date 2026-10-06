package loan

import (
	"testing"
	"time"
)

func TestDefaultReportingDateUsesMonthEnd(t *testing.T) {
	jakarta := time.FixedZone("Jakarta", 7*60*60)
	for now, want := range map[string]string{
		"2026-10-06T10:00:00+07:00": "2026-09-30", // mid-month -> previous month end
		"2026-10-30T23:59:00+07:00": "2026-09-30", // day before month end
		"2026-10-31T08:00:00+07:00": "2026-10-31", // month end -> today
		"2026-10-30T17:30:00Z":      "2026-10-31", // UTC instant already month end in Jakarta
		"2026-01-15T00:00:00+07:00": "2025-12-31", // crosses year
		"2026-03-10T00:00:00+07:00": "2026-02-28", // short February
		"2028-02-29T00:00:00+07:00": "2028-02-29", // leap-year month end
	} {
		parsed, err := time.Parse(time.RFC3339, now)
		if err != nil {
			t.Fatal(err)
		}
		if got := DefaultReportingDate(parsed, jakarta).String(); got != want {
			t.Errorf("DefaultReportingDate(%s) = %s, want %s", now, got, want)
		}
	}
}
