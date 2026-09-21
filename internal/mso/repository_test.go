package mso

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ibldzn/trs/internal/loan"
	"github.com/jmoiron/sqlx"
)

type queryCall struct {
	query string
	args  []driver.NamedValue
}

type queryResult struct {
	columns []string
	rows    [][]driver.Value
	err     error
}

type queryConnection struct {
	results []queryResult
	calls   []queryCall
}

func (connection *queryConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (connection *queryConnection) Close() error { return nil }
func (connection *queryConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}
func (connection *queryConnection) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	index := len(connection.calls)
	connection.calls = append(connection.calls, queryCall{query: query, args: args})
	if index >= len(connection.results) {
		return nil, errors.New("unexpected query")
	}
	result := connection.results[index]
	if result.err != nil {
		return nil, result.err
	}
	return &queryRows{columns: result.columns, rows: result.rows}, nil
}

type queryConnector struct{ connection *queryConnection }

func (connector *queryConnector) Connect(context.Context) (driver.Conn, error) {
	return connector.connection, nil
}
func (*queryConnector) Driver() driver.Driver { return queryDriver{} }

type queryDriver struct{}

func (queryDriver) Open(string) (driver.Conn, error) { return nil, errors.New("open not supported") }

type queryRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
}

func (rows *queryRows) Columns() []string { return rows.columns }
func (rows *queryRows) Close() error      { return nil }
func (rows *queryRows) Next(values []driver.Value) error {
	if rows.index >= len(rows.rows) {
		return io.EOF
	}
	copy(values, rows.rows[rows.index])
	rows.index++
	return nil
}

func newQueryRepository(t *testing.T, results ...queryResult) (*Repository, *queryConnection) {
	t.Helper()
	connection := &queryConnection{results: results}
	database := sqlx.NewDb(sql.OpenDB(&queryConnector{connection: connection}), "mysql")
	t.Cleanup(func() { _ = database.Close() })
	return NewRepository(database, time.UTC), connection
}

func assertQuery(t *testing.T, call queryCall, wantQuery string, wantArgument string) {
	t.Helper()
	if got := strings.Join(strings.Fields(call.query), " "); got != strings.Join(strings.Fields(wantQuery), " ") {
		t.Fatalf("query = %q", got)
	}
	if len(call.args) != 1 || call.args[0].Value != wantArgument {
		t.Fatalf("query arguments = %+v, want %q", call.args, wantArgument)
	}
}

func TestDebtorTypeByAlternateCIFFixedQuery(t *testing.T) {
	const query = `
		SELECT
			debitur_golongan2 AS debtor_type
		FROM
			data_nasabah_badan
		WHERE
			REPLACE(REPLACE(nasabah_master, '.', ''), '#', '') = ?`

	repository, connection := newQueryRepository(t, queryResult{columns: []string{"debtor_type"}, rows: [][]driver.Value{{"0201"}}})
	got, err := repository.DebtorTypeByAlternateCIF(context.Background(), "alternate-cif")
	if err != nil || got != "0201" {
		t.Fatalf("debtor type = %q, %v", got, err)
	}
	assertQuery(t, connection.calls[0], query, "alternate-cif")

	t.Run("missing", func(t *testing.T) {
		repository, _ := newQueryRepository(t, queryResult{columns: []string{"debtor_type"}})
		_, err := repository.DebtorTypeByAlternateCIF(context.Background(), "alternate-cif")
		if !errors.Is(err, loan.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("database failure", func(t *testing.T) {
		repository, _ := newQueryRepository(t, queryResult{err: errors.New("database failed")})
		_, err := repository.DebtorTypeByAlternateCIF(context.Background(), "alternate-cif")
		if !errors.Is(err, loan.ErrMSOUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestOpeningStateFixedInterestLookup(t *testing.T) {
	const query = `
		SELECT
			kre_sistem_bunga AS interest_type
		FROM
			data_kredit_master
		WHERE
			kre_rekening = ?`
	state := queryResult{
		columns: []string{"principal_outstanding", "principal_due", "interest_due", "collectability_bi"},
		rows:    [][]driver.Value{{"100", "10", "5", "L"}},
	}
	cutoff := loan.NewDate(time.Date(2025, 10, 12, 0, 0, 0, 0, time.UTC), time.UTC)

	repository, connection := newQueryRepository(t, state, queryResult{columns: []string{"interest_type"}, rows: [][]driver.Value{{" 10 "}}})
	got, err := repository.OpeningState(context.Background(), " account ", cutoff)
	if err != nil || got.InterestType != "10" {
		t.Fatalf("opening state interest type = %q, %v", got.InterestType, err)
	}
	assertQuery(t, connection.calls[1], query, "account")

	t.Run("missing", func(t *testing.T) {
		repository, _ := newQueryRepository(t, state, queryResult{columns: []string{"interest_type"}})
		_, err := repository.OpeningState(context.Background(), "account", cutoff)
		if !errors.Is(err, loan.ErrNotFound) || !errors.Is(err, loan.ErrHistoricalEvidence) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("database failure", func(t *testing.T) {
		repository, _ := newQueryRepository(t, state, queryResult{err: errors.New("database failed")})
		_, err := repository.OpeningState(context.Background(), "account", cutoff)
		if !errors.Is(err, loan.ErrMSOUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestParseCollectabilityBI(t *testing.T) {
	for input, want := range map[string]int{
		"1": 1, "L": 1,
		"2": 2, "DPK": 2, "DP": 2,
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
