package loan

import "testing"

func TestMoneyScannerRejectsMissingAndMalformedEvidence(t *testing.T) {
	var value Money
	if err := value.Scan(nil); err == nil {
		t.Fatal("NULL money accepted as zero")
	}
	if err := value.Scan([]byte("not-money")); err == nil {
		t.Fatal("malformed money accepted as zero")
	}
}

func TestExactDecimalPreservesContractRatePrecision(t *testing.T) {
	for _, raw := range []string{"0", "12.34567890123456789", "-0.125"} {
		got, err := MustMoney(raw).ExactDecimal()
		if err != nil || got != raw {
			t.Fatalf("ExactDecimal(%q)=%q, %v", raw, got, err)
		}
	}
}
