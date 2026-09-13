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
