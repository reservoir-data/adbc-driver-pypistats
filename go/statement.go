package pypistats

import (
	"context"
	"strings"

	"github.com/adbc-drivers/driverbase-go/driverbase"
	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// statementImpl treats the "SQL query" as a table name, optionally written
// as "SELECT * FROM <table>" for tools that insist on SQL.
type statementImpl struct {
	driverbase.StatementImplBase
	conn     *connectionImpl
	query    string
	packages []string
}

func (s *statementImpl) SetSqlQuery(_ context.Context, query string) error {
	s.query = query
	return nil
}

func (s *statementImpl) SetOption(ctx context.Context, key, val string) error {
	if key == OptionPackages {
		s.packages = parsePackages(val)
		return nil
	}
	return s.StatementImplBase.SetOption(ctx, key, val)
}

func (s *statementImpl) GetOption(ctx context.Context, key string) (string, error) {
	if key == OptionPackages {
		return strings.Join(s.packages, ","), nil
	}
	return s.StatementImplBase.GetOption(ctx, key)
}

// parseTableQuery accepts "<table>" or "SELECT * FROM <table>" (case-insensitive,
// optional trailing semicolon).
func parseTableQuery(q string) string {
	q = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(q), ";"))
	fields := strings.Fields(q)
	if len(fields) == 4 &&
		strings.EqualFold(fields[0], "select") && fields[1] == "*" && strings.EqualFold(fields[2], "from") {
		q = fields[3]
	}
	// Allow qualified names: pypistats.api.<table>
	if i := strings.LastIndexByte(q, '.'); i >= 0 {
		q = q[i+1:]
	}
	return strings.Trim(q, `"`)
}

func (s *statementImpl) resolve() (table, error) {
	if strings.TrimSpace(s.query) == "" {
		return table{}, s.ErrorHelper.InvalidState("no query set; use one of %v", tableNames())
	}
	name := parseTableQuery(s.query)
	t, ok := lookupTable(name)
	if !ok {
		return table{}, s.ErrorHelper.NotFound("unknown table %q; available tables: %v", name, tableNames())
	}
	return t, nil
}

func (s *statementImpl) ExecuteSchema(context.Context) (*arrow.Schema, error) {
	if _, err := s.resolve(); err != nil {
		return nil, err
	}
	return categorySchema, nil
}

func (s *statementImpl) ExecuteQuery(ctx context.Context) (array.RecordReader, int64, error) {
	t, err := s.resolve()
	if err != nil {
		return nil, -1, err
	}
	if len(s.packages) == 0 {
		return nil, -1, s.ErrorHelper.InvalidState("no packages configured; set %s", OptionPackages)
	}
	rdr := newDownloadsReader(ctx, s.conn.Alloc, &s.ErrorHelper, s.conn.client, t, s.packages, s.conn.cfg.maxConcurrency)
	return rdr, -1, nil
}

func (s *statementImpl) ExecuteUpdate(context.Context) (int64, error) {
	return -1, s.ErrorHelper.NotImplemented("pypistats is read-only")
}

func (s *statementImpl) Bind(context.Context, arrow.RecordBatch) error {
	return s.ErrorHelper.NotImplemented("pypistats does not support parameters")
}

func (s *statementImpl) BindStream(context.Context, array.RecordReader) error {
	return s.ErrorHelper.NotImplemented("pypistats does not support parameters")
}

func (s *statementImpl) Prepare(context.Context) error {
	_, err := s.resolve()
	return err
}

var _ adbc.StatementExecuteSchema = (*statementImpl)(nil)
