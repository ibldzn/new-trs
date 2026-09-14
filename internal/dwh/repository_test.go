package dwh

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
)

type databaseFake struct {
	getRow      positionRow
	selectRows  []collectabilityRow
	getQuery    string
	selectQuery string
	getArgs     []any
	selectArgs  []any
}

func (database *databaseFake) GetContext(_ context.Context, destination any, query string, args ...any) error {
	database.getQuery = query
	database.getArgs = args
	*(destination.(*positionRow)) = database.getRow
	return nil
}

func (database *databaseFake) SelectContext(_ context.Context, destination any, query string, args ...any) error {
	database.selectQuery = query
	database.selectArgs = args
	*(destination.(*[]collectabilityRow)) = append([]collectabilityRow(nil), database.selectRows...)
	return nil
}

func TestExactPositionQueriesPrimaryAccountThenDate(t *testing.T) {
	database := &databaseFake{getRow: positionRow{
		AsOf: time.Date(2026, time.September, 13, 0, 0, 0, 0, time.UTC), AccountNumber: "primary",
		PrincipalOutstanding: loan.MustMoney("100"), CollectabilityBI: 2,
	}}
	repository := &Repository{database: database, location: time.UTC}
	asOf, _ := loan.ParseDate("2026-09-13", time.UTC)
	position, err := repository.ExactPosition(context.Background(), "primary", asOf)
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Join(strings.Fields(database.getQuery), " ")
	if !strings.Contains(query, "WHERE no_rekening = ? AND as_of_date = ?") || strings.Contains(query, "business_key_hash") {
		t.Fatalf("query = %s", query)
	}
	if !reflect.DeepEqual(database.getArgs, []any{"primary", "2026-09-13"}) {
		t.Fatalf("args = %#v", database.getArgs)
	}
	if position.AccountNumber != "primary" || position.Source != loan.SourceDWH {
		t.Fatalf("position = %+v", position)
	}
}

func TestCollectabilityTimelineQueriesSparseAccountDateRange(t *testing.T) {
	database := &databaseFake{selectRows: []collectabilityRow{
		{Date: time.Date(2025, time.October, 14, 0, 0, 0, 0, time.UTC), Value: 2},
		{Date: time.Date(2025, time.October, 19, 0, 0, 0, 0, time.UTC), Value: 3},
	}}
	repository := &Repository{database: database, location: time.UTC}
	from, _ := loan.ParseDate("2025-10-13", time.UTC)
	to, _ := loan.ParseDate("2025-10-20", time.UTC)
	points, err := repository.CollectabilityTimeline(context.Background(), "primary", from, to)
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Join(strings.Fields(database.selectQuery), " ")
	for _, clause := range []string{"WHERE no_rekening = ?", "as_of_date >= ?", "as_of_date <= ?", "ORDER BY as_of_date ASC"} {
		if !strings.Contains(query, clause) {
			t.Fatalf("query missing %q: %s", clause, query)
		}
	}
	if strings.Contains(query, "business_key_hash") || strings.Contains(query, "RECURSIVE") {
		t.Fatalf("query = %s", query)
	}
	if !reflect.DeepEqual(database.selectArgs, []any{"primary", "2025-10-13", "2025-10-20"}) {
		t.Fatalf("args = %#v", database.selectArgs)
	}
	if len(points) != 2 || points[0].Date.String() != "2025-10-14" || points[1].Date.String() != "2025-10-19" {
		t.Fatalf("points = %+v", points)
	}
}

func TestCollectabilityTimelineRejectsConflictingDuplicateDate(t *testing.T) {
	day := time.Date(2025, time.October, 14, 0, 0, 0, 0, time.UTC)
	database := &databaseFake{selectRows: []collectabilityRow{{Date: day, Value: 2}, {Date: day, Value: 3}}}
	repository := &Repository{database: database, location: time.UTC}
	from, _ := loan.ParseDate("2025-10-13", time.UTC)
	to, _ := loan.ParseDate("2025-10-20", time.UTC)
	_, err := repository.CollectabilityTimeline(context.Background(), "primary", from, to)
	if !errors.Is(err, loan.ErrHistoricalEvidence) {
		t.Fatalf("error = %v", err)
	}
}
