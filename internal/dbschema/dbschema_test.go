package dbschema

import (
	"testing"
	"testing/fstest"
)

func TestVersionsMatchMigrationFiles(t *testing.T) {
	versions, err := Versions()
	if err != nil || len(versions) == 0 {
		t.Fatalf("versions=%v err=%v", versions, err)
	}
	for index := 1; index < len(versions); index++ {
		if versions[index] <= versions[index-1] {
			t.Fatalf("versions not strictly ascending: %v", versions)
		}
	}
	if _, err := versionsFrom(fstest.MapFS{"create_things.sql": {}}); err == nil {
		t.Fatal("unversioned migration accepted")
	}
}

func TestValidateMigrationPrefix(t *testing.T) {
	versions := []int64{1, 2, 3}
	complete := MigrationState{TableExists: true, Records: []MigrationRecord{{Version: 0, Applied: true}}}
	for _, version := range versions {
		complete.Records = append(complete.Records, MigrationRecord{Version: version, Applied: true})
	}
	if count, err := ValidateMigrationPrefix(complete, versions); err != nil || count != len(versions) {
		t.Fatalf("complete count=%d error=%v", count, err)
	}
	prefix := MigrationState{TableExists: true, Records: append([]MigrationRecord(nil), complete.Records[:len(complete.Records)-1]...)}
	if count, err := ValidateMigrationPrefix(prefix, versions); err != nil || count != len(versions)-1 {
		t.Fatalf("prefix count=%d error=%v", count, err)
	}
	unknown := MigrationState{TableExists: true, Records: append([]MigrationRecord(nil), prefix.Records...)}
	unknown.Records = append(unknown.Records, MigrationRecord{Version: 999, Applied: true})
	if count, err := ValidateMigrationPrefix(unknown, versions); err != nil || count != len(versions)-1 {
		t.Fatalf("unknown version must be skipped: count=%d error=%v", count, err)
	}
	gapped := MigrationState{TableExists: true, Records: append([]MigrationRecord(nil), complete.Records...)}
	gapped.Records = append(gapped.Records, MigrationRecord{Version: versions[1], Applied: false})
	if _, err := ValidateMigrationPrefix(gapped, versions); err == nil {
		t.Fatal("gapped migration history accepted")
	}
}
