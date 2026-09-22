package access

import (
	"reflect"
	"testing"
)

func TestBootstrapUsernamesNormalizeDeduplicateAndRejectEmpty(t *testing.T) {
	got, err := bootstrapUsernames(" Alice, bob,ALICE,Alice ")
	if err != nil || !reflect.DeepEqual(got, []string{"Alice", "bob", "ALICE"}) {
		t.Fatalf("usernames=%v err=%v", got, err)
	}
	if _, err := bootstrapUsernames(" , "); err == nil {
		t.Fatal("empty bootstrap configuration passed")
	}
}
