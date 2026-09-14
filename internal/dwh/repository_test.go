package dwh

import "testing"

func TestAccountBusinessKeyMatchesDWHV2Identity(t *testing.T) {
	const want = "551660f80827d3fe4f86b143d657a462e36f1767c35ba15ff1b75a78dd411f43"
	if got := accountBusinessKey(" 3000010000000061 "); got != want {
		t.Fatalf("business key = %q, want %q", got, want)
	}
}
