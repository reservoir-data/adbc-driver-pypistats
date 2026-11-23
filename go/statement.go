package pypistats

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/adbc-drivers/driverbase-go/driverbase"
	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// statementImpl treats the "SQL query" as a table name, optionally written
// as "SELECT * FROM <table> [WHERE package ...]" for tools that insist on SQL.
// packages is the default scope, used when the query has no package predicate.
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

// resolve parses the query and looks up its table. The returned packages are
// the query's package predicate if it has one, else the configured default
// scope.
func (s *statementImpl) resolve() (table, []string, []int, error) {
	if strings.TrimSpace(s.query) == "" {
		return table{}, nil, nil, s.ErrorHelper.InvalidState("no query set; use one of %v", tableNames())
	}
	q, err := parseQuery(s.query)
	if err != nil {
		var qe *queryError
		if errors.As(err, &qe) && qe.unsupported {
			return table{}, nil, nil, s.ErrorHelper.NotImplemented("%s", qe.msg)
		}
		return table{}, nil, nil, s.ErrorHelper.InvalidArgument("%s", err.Error())
	}
	t, ok := lookupTable(q.table)
	if !ok {
		return table{}, nil, nil, s.ErrorHelper.NotFound("unknown table %q; available tables: %v", q.table, tableNames())
	}
	var cols []int
	for _, name := range q.columns {
		i := slices.IndexFunc(categorySchema.Fields(), func(f arrow.Field) bool { return strings.EqualFold(f.Name, name) })
		if i < 0 {
			return table{}, nil, nil, s.ErrorHelper.NotFound("unknown column %q in table %s", name, t.Name)
		}
		cols = append(cols, i)
	}
	if q.packages != nil {
		return t, q.packages, cols, nil
	}
	return t, s.packages, cols, nil
}

// projectSchema returns categorySchema narrowed to cols, or all of it if nil.
func projectSchema(cols []int) *arrow.Schema {
	if cols == nil {
		return categorySchema
	}
	fields := make([]arrow.Field, len(cols))
	for i, c := range cols {
		fields[i] = categorySchema.Field(c)
	}
	return arrow.NewSchema(fields, nil)
}

func (s *statementImpl) ExecuteSchema(context.Context) (*arrow.Schema, error) {
	_, _, cols, err := s.resolve()
	if err != nil {
		return nil, err
	}
	return projectSchema(cols), nil
}

func (s *statementImpl) ExecuteQuery(ctx context.Context) (array.RecordReader, int64, error) {
	t, packages, cols, err := s.resolve()
	if err != nil {
		return nil, -1, err
	}
	if len(packages) == 0 {
		return nil, -1, s.ErrorHelper.InvalidState(
			"no packages selected; add WHERE package = '<name>' (or package IN (...)) or set %s", OptionPackages)
	}
	rdr := newDownloadsReader(ctx, s.conn.Alloc, &s.ErrorHelper, s.conn.client, t, packages, cols, s.conn.cfg.effectiveMaxConcurrency())
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
	_, _, _, err := s.resolve()
	return err
}

var _ adbc.StatementExecuteSchema = (*statementImpl)(nil)
