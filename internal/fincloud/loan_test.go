package fincloud

import "testing"

func TestNormalizeDecimalPreservesSALAKSeparatorRules(t *testing.T) {
	for raw, want := range map[string]string{
		"1.234,56":     "1234.56",
		"1,234.56":     "1234.56",
		"1.234.567":    "1234567",
		"1234.567.890": "1234567.890",
		"1234.56":      "1234.56",
	} {
		got, err := NormalizeDecimal(raw)
		if err != nil || got != want {
			t.Fatalf("NormalizeDecimal(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := NormalizeDecimal("12x"); err == nil {
		t.Fatal("malformed decimal accepted")
	}
}
