package pypistats

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pythonMinorBody = `{"data": [
  {"category": "3.12", "date": "2026-11-01", "downloads": 10},
  {"category": "null", "date": "2026-11-01", "downloads": 2},
  {"category": "3.13", "date": "2026-11-02", "downloads": 7}
], "package": "%s", "type": "python_minor_downloads"}`

const systemBody = `{"data": [
  {"category": "linux", "date": "2026-11-01", "downloads": 100},
  {"category": "darwin", "date": "2026-11-01", "downloads": 20}
], "package": "%s", "type": "system_downloads"}`

type fakeServer struct {
	*httptest.Server
	hits      atomic.Int64
	throttle  atomic.Int64 // respond 429 this many times before succeeding
	delay     time.Duration
	userAgent atomic.Value
}

func newFakeServer(t *testing.T) *fakeServer {
	fs := &fakeServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.hits.Add(1)
		fs.userAgent.Store(r.UserAgent())
		if fs.throttle.Add(-1) >= 0 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		if fs.delay > 0 {
			select {
			case <-time.After(fs.delay):
			case <-r.Context().Done():
				return
			}
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/packages/"), "/")
		if len(parts) != 2 || parts[0] == "does-not-exist" {
			http.NotFound(w, r)
			return
		}
		pkg, endpoint := parts[0], parts[1]
		w.Header().Set("Content-Type", "application/json")
		switch endpoint {
		case "python_minor":
			fmt.Fprintf(w, pythonMinorBody, pkg)
		case "system":
			fmt.Fprintf(w, systemBody, pkg)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fs.Close)
	return fs
}

type harness struct {
	alloc *memory.CheckedAllocator
	db    adbc.DatabaseWithContext
	conn  adbc.ConnectionWithContext
}

func setup(t *testing.T, fs *fakeServer, opts map[string]string) *harness {
	t.Helper()
	ctx := context.Background()
	alloc := memory.NewCheckedAllocator(memory.NewGoAllocator())
	all := map[string]string{
		OptionBaseURL:  fs.URL + "/api",
		OptionPackages: "requests, numpy",
	}
	for k, v := range opts {
		all[k] = v
	}
	db, err := NewDriver(alloc).NewDatabaseWithContext(ctx, all)
	require.NoError(t, err)
	conn, err := db.Open(ctx)
	require.NoError(t, err)
	h := &harness{alloc: alloc, db: db, conn: conn}
	t.Cleanup(func() {
		require.NoError(t, conn.Close(ctx))
		require.NoError(t, db.Close(ctx))
		alloc.AssertSize(t, 0)
	})
	return h
}

func (h *harness) query(t *testing.T, q string, stmtOpts map[string]string) (array.RecordReader, error) {
	t.Helper()
	ctx := context.Background()
	stmt, err := h.conn.NewStatement(ctx)
	require.NoError(t, err)
	defer stmt.Close(ctx)
	for k, v := range stmtOpts {
		require.NoError(t, stmt.SetOption(ctx, k, v))
	}
	require.NoError(t, stmt.SetSqlQuery(ctx, q))
	rdr, _, err := stmt.ExecuteQuery(ctx)
	return rdr, err
}

type row struct {
	pkg, category string
	date          string
	downloads     uint32
}

func collect(t *testing.T, rdr array.RecordReader) []row {
	t.Helper()
	defer rdr.Release()
	var out []row
	for rdr.Next() {
		rec := rdr.RecordBatch()
		pkg := rec.Column(0).(*array.String)
		cat := rec.Column(1).(*array.String)
		date := rec.Column(2).(*array.Date32)
		dl := rec.Column(3).(*array.Uint32)
		for i := range int(rec.NumRows()) {
			require.True(t, cat.IsValid(i), "category must not be null")
			out = append(out, row{pkg: pkg.Value(i), category: cat.Value(i), date: date.Value(i).FormattedString(), downloads: dl.Value(i)})
		}
	}
	require.NoError(t, rdr.Err())
	return out
}

func adbcStatus(t *testing.T, err error) adbc.Status {
	t.Helper()
	var aerr adbc.Error
	require.True(t, errors.As(err, &aerr), "expected adbc.Error, got %T: %v", err, err)
	return aerr.Code
}

func TestPythonMinor(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, nil)

	rdr, err := h.query(t, "python_minor", nil)
	require.NoError(t, err)
	assert.True(t, rdr.Schema().Equal(categorySchema))

	assert.Equal(t, []row{
		{"requests", "3.12", "2026-11-01", 10},
		{"requests", "null", "2026-11-01", 2},
		{"requests", "3.13", "2026-11-02", 7},
		{"numpy", "3.12", "2026-11-01", 10},
		{"numpy", "null", "2026-11-01", 2},
		{"numpy", "3.13", "2026-11-02", 7},
	}, collect(t, rdr))
	assert.Equal(t, defaultUserAgent, fs.userAgent.Load())
}

func TestSystemWithSQLAndStatementPackages(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, nil)

	rdr, err := h.query(t, "SELECT * FROM pypistats.api.SYSTEM;", map[string]string{OptionPackages: "python-backoff"})
	require.NoError(t, err)
	assert.Equal(t, []row{
		{"python-backoff", "linux", "2026-11-01", 100},
		{"python-backoff", "darwin", "2026-11-01", 20},
	}, collect(t, rdr))
}

func TestWherePackage(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, nil) // default scope is "requests, numpy"

	rdr, err := h.query(t, "SELECT * FROM system WHERE package = 'urllib3'", nil)
	require.NoError(t, err)
	assert.Equal(t, []row{
		{"urllib3", "linux", "2026-11-01", 100},
		{"urllib3", "darwin", "2026-11-01", 20},
	}, collect(t, rdr))
	assert.EqualValues(t, 1, fs.hits.Load())
}

func TestWherePackageIn(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, nil)

	// The predicate overrides both the database default and the statement option;
	// duplicates are fetched once and order is preserved.
	rdr, err := h.query(t,
		`select * from pypistats.api."system" where PACKAGE in ('urllib3', 'requests', 'urllib3');`,
		map[string]string{OptionPackages: "ignored"})
	require.NoError(t, err)
	rows := collect(t, rdr)
	require.Len(t, rows, 4)
	assert.Equal(t, "urllib3", rows[0].pkg)
	assert.Equal(t, "requests", rows[2].pkg)
	assert.EqualValues(t, 2, fs.hits.Load())
}

func TestWhereQuoteEscapes(t *testing.T) {
	q, err := parseQuery("SELECT * FROM system WHERE package = 'it''s'")
	require.NoError(t, err)
	assert.Equal(t, []string{"it's"}, q.packages)
}

func TestNoPackagesSelected(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, map[string]string{OptionPackages: ""})

	_, err := h.query(t, "SELECT * FROM system", nil)
	assert.Equal(t, adbc.StatusInvalidState, adbcStatus(t, err))
	assert.Contains(t, err.Error(), "WHERE package")

	// A predicate is enough without any default scope.
	rdr, err := h.query(t, "SELECT * FROM system WHERE package = 'requests'", nil)
	require.NoError(t, err)
	assert.Len(t, collect(t, rdr), 2)
}

func TestColumnProjection(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, nil)

	rdr, err := h.query(t, `SELECT "downloads", "package" FROM system WHERE package = 'requests'`, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"downloads", "package"}, []string{rdr.Schema().Field(0).Name, rdr.Schema().Field(1).Name})
	assert.Equal(t, 2, rdr.Schema().NumFields())
	defer rdr.Release()
	rows := 0
	for rdr.Next() {
		rec := rdr.RecordBatch()
		require.EqualValues(t, 2, rec.NumCols())
		dl := rec.Column(0).(*array.Uint32)
		pkg := rec.Column(1).(*array.String)
		for i := range int(rec.NumRows()) {
			assert.Equal(t, "requests", pkg.Value(i))
			assert.Positive(t, dl.Value(i))
			rows++
		}
	}
	require.NoError(t, rdr.Err())
	assert.Equal(t, 2, rows)

	_, err = h.query(t, "SELECT nope FROM system", nil)
	assert.Equal(t, adbc.StatusNotFound, adbcStatus(t, err))
}

func TestUnsupportedSQL(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, nil)

	for _, q := range []string{
		"SELECT * FROM system WHERE category = 'linux'",
		"SELECT * FROM system WHERE package LIKE 'a%'",
		"SELECT * FROM system WHERE package = 'a' AND category = 'linux'",
		"SELECT * FROM system WHERE package = 'a' OR package = 'b'",
		"SELECT * FROM system WHERE package = 'a' LIMIT 1",
		"SELECT * FROM system ORDER BY date",
	} {
		_, err := h.query(t, q, nil)
		assert.Equal(t, adbc.StatusNotImplemented, adbcStatus(t, err), q)
	}

	for _, q := range []string{
		"SELECT * FROM system WHERE package = ",
		"SELECT * FROM system WHERE package IN ()",
		"SELECT * FROM system WHERE package IN ('a'",
		"SELECT * FROM system WHERE package = 'a",
		"SELECT * FROM system WHERE package = 1",
		"SELECT * FROM system WHERE package = ''",
		"DELETE FROM system",
	} {
		_, err := h.query(t, q, nil)
		assert.Equal(t, adbc.StatusInvalidArgument, adbcStatus(t, err), q)
	}
	assert.Zero(t, fs.hits.Load())
}

func TestRetriesOn429(t *testing.T) {
	fs := newFakeServer(t)
	fs.throttle.Store(2)
	h := setup(t, fs, map[string]string{OptionPackages: "requests"})

	rdr, err := h.query(t, "system", nil)
	require.NoError(t, err)
	assert.Len(t, collect(t, rdr), 2)
	assert.EqualValues(t, 3, fs.hits.Load())
}

func TestRetriesExhausted(t *testing.T) {
	fs := newFakeServer(t)
	fs.throttle.Store(100)
	h := setup(t, fs, map[string]string{OptionPackages: "requests", OptionMaxRetries: "1"})

	rdr, err := h.query(t, "system", nil)
	require.NoError(t, err)
	defer rdr.Release()
	assert.False(t, rdr.Next())
	assert.Equal(t, adbc.StatusIO, adbcStatus(t, rdr.Err()))
	assert.EqualValues(t, 2, fs.hits.Load())
}

func TestPackageNotFound(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, map[string]string{OptionPackages: "requests,does-not-exist,numpy"})

	rdr, err := h.query(t, "python_minor", nil)
	require.NoError(t, err)
	defer rdr.Release()
	require.True(t, rdr.Next()) // requests succeeds first
	assert.False(t, rdr.Next())
	assert.Equal(t, adbc.StatusNotFound, adbcStatus(t, rdr.Err()))
	assert.Contains(t, rdr.Err().Error(), "does-not-exist")
}

func TestEarlyReleaseFreesPendingBatches(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, map[string]string{OptionPackages: "a,b,c,d", OptionMaxConcurrency: "4"})

	rdr, err := h.query(t, "system", nil)
	require.NoError(t, err)
	require.True(t, rdr.Next())
	rdr.Release()
	assert.Zero(t, h.alloc.CurrentAlloc())
}

func TestQueryErrors(t *testing.T) {
	fs := newFakeServer(t)
	h := setup(t, fs, nil)

	_, err := h.query(t, "overall", nil)
	assert.Equal(t, adbc.StatusNotFound, adbcStatus(t, err))

	_, err = h.query(t, "system", map[string]string{OptionPackages: " , "})
	assert.Equal(t, adbc.StatusInvalidState, adbcStatus(t, err))
	assert.Zero(t, fs.hits.Load())
}

func TestTypedOptions(t *testing.T) {
	ctx := context.Background()
	db, err := NewDriver(memory.DefaultAllocator).NewDatabaseWithContext(ctx, nil)
	require.NoError(t, err)
	defer db.Close(ctx)
	opts := db.(adbc.GetSetOptionsWithContext)

	// Profiles pass TOML integers/floats through SetOptionInt/SetOptionDouble.
	require.NoError(t, opts.SetOptionInt(ctx, OptionMaxConcurrency, 5))
	require.NoError(t, opts.SetOptionInt(ctx, OptionRequestTimeout, 7))
	require.NoError(t, opts.SetOptionDouble(ctx, OptionRequestTimeout, 2.5))
	require.NoError(t, opts.SetOption(ctx, OptionPackages, "a,b"))

	v, err := opts.GetOptionInt(ctx, OptionMaxConcurrency)
	require.NoError(t, err)
	assert.EqualValues(t, 5, v)
	d, err := opts.GetOptionDouble(ctx, OptionRequestTimeout)
	require.NoError(t, err)
	assert.Equal(t, 2.5, d)
	s, err := opts.GetOption(ctx, OptionPackages)
	require.NoError(t, err)
	assert.Equal(t, "a,b", s)

	assert.Equal(t, adbc.StatusInvalidArgument, adbcStatus(t, opts.SetOptionInt(ctx, OptionMaxConcurrency, 0)))
	assert.Equal(t, adbc.StatusInvalidArgument, adbcStatus(t, opts.SetOption(ctx, adbc.OptionKeyURI, "https://example.com")))
	assert.Equal(t, adbc.StatusNotImplemented, adbcStatus(t, opts.SetOption(ctx, "pypistats.bogus", "x")))
}

// The driver manager calls set_autocommit(False) on connect and warns that
// the connection is not DB-API 2.0 compliant if that fails.
func TestAutocommitIsNoOp(t *testing.T) {
	ctx := context.Background()
	h := setup(t, newFakeServer(t), nil)
	opts := h.conn.(adbc.GetSetOptionsWithContext)
	for _, v := range []string{adbc.OptionValueDisabled, adbc.OptionValueEnabled, adbc.OptionValueDisabled} {
		require.NoError(t, opts.SetOption(ctx, adbc.OptionKeyAutoCommit, v))
		got, err := opts.GetOption(ctx, adbc.OptionKeyAutoCommit)
		require.NoError(t, err)
		assert.Equal(t, v, got)
	}

	// With autocommit off, commit/rollback must succeed.
	require.NoError(t, h.conn.Commit(ctx))
	require.NoError(t, h.conn.Rollback(ctx))
}

func TestMetadata(t *testing.T) {
	ctx := context.Background()
	fs := newFakeServer(t)
	h := setup(t, fs, nil)

	schema, err := h.conn.GetTableSchema(ctx, nil, nil, "python_minor")
	require.NoError(t, err)
	assert.True(t, schema.Equal(categorySchema))
	_, err = h.conn.GetTableSchema(ctx, nil, nil, "overall")
	assert.Equal(t, adbc.StatusNotFound, adbcStatus(t, err))

	pattern := "py%"
	rdr, err := h.conn.GetObjects(ctx, adbc.ObjectDepthAll, nil, nil, &pattern, nil, nil)
	require.NoError(t, err)
	defer rdr.Release()

	var names []string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		catalogs := rec.Column(0).(*array.String)
		schemas := rec.Column(1).(*array.List)
		for i := range int(rec.NumRows()) {
			assert.Equal(t, catalogName, catalogs.Value(i))
			schemaStructs := schemas.ListValues().(*array.Struct)
			tablesList := schemaStructs.Field(1).(*array.List)
			tableStructs := tablesList.ListValues().(*array.Struct)
			tableNameArr := tableStructs.Field(0).(*array.String)
			for j := range tableNameArr.Len() {
				names = append(names, tableNameArr.Value(j))
			}

			constraintsList := tableStructs.Field(3).(*array.List)
			constraintStructs := constraintsList.ListValues().(*array.Struct)
			require.Equal(t, 1, constraintStructs.Len())
			assert.Equal(t, "PRIMARY KEY", constraintStructs.Field(1).(*array.String).Value(0))
			colNames := constraintStructs.Field(2).(*array.List)
			start, end := colNames.ValueOffsets(0)
			colNameValues := colNames.ListValues().(*array.String)
			var pk []string
			for k := start; k < end; k++ {
				pk = append(pk, colNameValues.Value(int(k)))
			}
			assert.Equal(t, []string{"package", "category", "date"}, pk)
		}
	}
	require.NoError(t, rdr.Err())
	assert.Equal(t, []string{"python_minor"}, names)
}

func TestParseQueryProjection(t *testing.T) {
	q, err := parseQuery(`SELECT "package", "downloads" FROM "api"."python_minor" WHERE "package" = 'requests'`)
	require.NoError(t, err)
	assert.Equal(t, "python_minor", q.table)
	assert.Equal(t, []string{"package", "downloads"}, q.columns)
	assert.Equal(t, []string{"requests"}, q.packages)
}
