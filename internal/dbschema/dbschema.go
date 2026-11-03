// Package dbschema verifies that the database Goose history matches the
// migrations embedded in this binary.
package dbschema

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/ibldzn/trs/migrations"
)

type MigrationRecord struct {
	Version int64 `db:"version_id"`
	Applied bool  `db:"is_applied"`
}

type MigrationState struct {
	TableExists bool
	Records     []MigrationRecord
}

// Versions returns the embedded migration versions in ascending order.
func Versions() ([]int64, error) {
	return versionsFrom(migrations.Files)
}

func versionsFrom(files fs.FS) ([]int64, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	versions := make([]int64, 0, len(names))
	for _, name := range names {
		prefix, _, _ := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q has no numeric version prefix", name)
		}
		versions = append(versions, version)
	}
	slices.Sort(versions)
	return versions, nil
}

func ReadMigrationState(ctx context.Context, db *sqlx.DB) (MigrationState, error) {
	var tableCount int
	if err := db.GetContext(ctx, &tableCount, `SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='goose_db_version'`); err != nil {
		return MigrationState{}, fmt.Errorf("inspect Goose metadata: %w", err)
	}
	if tableCount == 0 {
		return MigrationState{}, nil
	}
	var records []MigrationRecord
	if err := db.SelectContext(ctx, &records, `SELECT version_id,is_applied FROM goose_db_version ORDER BY id`); err != nil {
		return MigrationState{}, fmt.Errorf("read Goose metadata: %w", err)
	}
	return MigrationState{TableExists: true, Records: records}, nil
}

// ValidateMigrationPrefix returns how many known versions are applied. The
// known versions must form a gap-free prefix.
func ValidateMigrationPrefix(state MigrationState, versions []int64) (int, error) {
	if !state.TableExists {
		return 0, nil
	}
	known := make(map[int64]bool, len(versions))
	for _, version := range versions {
		known[version] = true
	}
	latest := make(map[int64]bool, len(state.Records))
	for _, record := range state.Records {
		if record.Version == 0 {
			continue
		}
		// ponytail: unknown versions are skipped because the local DB is shared with trs-v2's
		// Goose history; make this an error again once new-trs owns its database alone.
		if !known[record.Version] {
			continue
		}
		latest[record.Version] = record.Applied
	}
	applied := 0
	missing := false
	for _, version := range versions {
		if latest[version] {
			if missing {
				return 0, fmt.Errorf("Goose history is not a canonical migration prefix")
			}
			applied++
		} else {
			missing = true
		}
	}
	return applied, nil
}

// VerifyRuntime fails unless every embedded migration has been applied.
func VerifyRuntime(ctx context.Context, db *sqlx.DB) error {
	versions, err := Versions()
	if err != nil {
		return err
	}
	state, err := ReadMigrationState(ctx, db)
	if err != nil {
		return err
	}
	applied, err := ValidateMigrationPrefix(state, versions)
	if err != nil {
		return err
	}
	if !state.TableExists || applied != len(versions) {
		return fmt.Errorf("database schema is at %d of %d migrations; run migrate up", applied, len(versions))
	}
	return nil
}
