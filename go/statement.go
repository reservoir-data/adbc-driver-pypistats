// Copyright (c) 2026 Edgar Ramírez-Mondragón
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//         http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
//
// Instead of a query, the statement may target a metadata collection or a
// vendor call (see call.go); setting any of the three replaces the others.
type statementImpl struct {
	driverbase.StatementImplBase
	conn       *connectionImpl
	query      string
	packages   []string
	collection *collection
	call       *call
	// params holds the bound parameter batches, owned by the statement.
	params []arrow.RecordBatch
}

func (s *statementImpl) SetSqlQuery(_ context.Context, query string) error {
	s.reset()
	s.query = query
	return nil
}

// reset clears the statement's target and any bound parameters.
func (s *statementImpl) reset() {
	s.query, s.collection, s.call = "", nil, nil
	s.releaseParams()
}

func (s *statementImpl) releaseParams() {
	for _, rec := range s.params {
		rec.Release()
	}
	s.params = nil
}

func (s *statementImpl) SetOption(ctx context.Context, key, val string) error {
	switch key {
	case OptionPackages:
		s.packages = parsePackages(val)
		return nil
	case OptionMetadataCollection:
		c, ok := lookupCollection(val)
		if !ok {
			return s.ErrorHelper.InvalidArgument("could not set %s: unknown collection %q, available: %v", key, val, collectionNames())
		}
		s.reset()
		s.collection = &c
		return nil
	case OptionStatementCall:
		c, ok := lookupCall(val)
		if !ok {
			return s.ErrorHelper.NotFound("could not set %s: unknown call %q, available: %v", key, val, callNames())
		}
		s.reset()
		s.call = &c
		return nil
	}
	return s.StatementImplBase.SetOption(ctx, key, val)
}

func (s *statementImpl) GetOption(ctx context.Context, key string) (string, error) {
	switch key {
	case OptionPackages:
		return strings.Join(s.packages, ","), nil
	case OptionMetadataCollection:
		if s.collection != nil {
			return s.collection.Name, nil
		}
		return "", nil
	case OptionStatementCall:
		if s.call != nil {
			return s.call.Name, nil
		}
		return "", nil
	}
	return s.StatementImplBase.GetOption(ctx, key)
}

func (s *statementImpl) Close(context.Context) error {
	s.releaseParams()
	return nil
}

// resolve parses the query and looks up its table. The returned packages are
// the query's package predicate if it has one, else the configured default
// scope.
func (s *statementImpl) resolve() (table, []string, []int, error) {
	if strings.TrimSpace(s.query) == "" {
		return table{}, nil, nil, s.ErrorHelper.InvalidState("could not execute: no query set, use one of %v", tableNames())
	}
	q, err := parseQuery(s.query)
	if err != nil {
		var qe *queryError
		if errors.As(err, &qe) && qe.unsupported {
			return table{}, nil, nil, s.ErrorHelper.NotImplemented("could not run query: %s", qe.msg)
		}
		return table{}, nil, nil, s.ErrorHelper.InvalidArgument("could not parse query: %s", err.Error())
	}
	t, ok := lookupTable(q.table)
	if !ok {
		return table{}, nil, nil, s.ErrorHelper.NotFound("could not find table: %q is unknown, available tables: %v", q.table, tableNames())
	}
	var cols []int
	for _, name := range q.columns {
		i := slices.IndexFunc(categorySchema.Fields(), func(f arrow.Field) bool { return strings.EqualFold(f.Name, name) })
		if i < 0 {
			return table{}, nil, nil, s.ErrorHelper.NotFound("could not find column: %q is not in table %s", name, t.Name)
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
	switch {
	case s.collection != nil:
		return s.collection.Schema, nil
	case s.call != nil:
		return s.call.Result, nil
	}
	_, _, cols, err := s.resolve()
	if err != nil {
		return nil, err
	}
	return projectSchema(cols), nil
}

func (s *statementImpl) ExecuteQuery(ctx context.Context) (array.RecordReader, int64, error) {
	switch {
	case s.collection != nil:
		rdr, err := s.collection.read(s.conn.Alloc)
		if err != nil {
			return nil, -1, s.ErrorHelper.WrapInternal(err, "could not read collection %s", s.collection.Name)
		}
		return rdr, -1, nil
	case s.call != nil:
		if s.params == nil {
			return nil, -1, s.ErrorHelper.InvalidState("could not execute %s: no parameters bound, bind a batch matching %v", s.call.Name, s.call.Params)
		}
		args, err := s.call.packageArgs(&s.ErrorHelper, s.params)
		if err != nil {
			return nil, -1, err
		}
		rdr, err := s.call.invoke(ctx, s.conn, &s.ErrorHelper, args)
		if err != nil {
			return nil, -1, err
		}
		return rdr, int64(len(args)), nil
	}
	t, packages, cols, err := s.resolve()
	if err != nil {
		return nil, -1, err
	}
	if len(packages) == 0 {
		return nil, -1, s.ErrorHelper.InvalidState(
			"could not execute: no packages selected, add WHERE package = '<name>' (or package IN (...)) or set %s", OptionPackages)
	}
	rdr := newDownloadsReader(ctx, s.conn.Alloc, &s.ErrorHelper, s.conn.client, t, packages, cols, s.conn.cfg.effectiveMaxConcurrency())
	return rdr, -1, nil
}

func (s *statementImpl) ExecuteUpdate(context.Context) (int64, error) {
	return -1, s.ErrorHelper.NotImplemented("could not execute update: driver is read-only")
}

// GetParameterSchema describes a call's inputs, or a collection's filters
// (none here). Plain table queries take no parameters.
func (s *statementImpl) GetParameterSchema(ctx context.Context) (*arrow.Schema, error) {
	switch {
	case s.collection != nil:
		return arrow.NewSchema(nil, nil), nil
	case s.call != nil:
		return s.call.Params, nil
	}
	return s.StatementImplBase.GetParameterSchema(ctx)
}

// Bind retains values; the caller keeps its own reference (the cgo wrapper
// releases it once Bind returns).
func (s *statementImpl) Bind(_ context.Context, values arrow.RecordBatch) error {
	if s.call == nil {
		return s.ErrorHelper.NotImplemented("could not bind parameters: only supported with %s", OptionStatementCall)
	}
	values.Retain()
	s.releaseParams()
	s.params = []arrow.RecordBatch{values}
	return nil
}

// BindStream drains and takes ownership of stream.
func (s *statementImpl) BindStream(_ context.Context, stream array.RecordReader) error {
	defer stream.Release()
	if s.call == nil {
		return s.ErrorHelper.NotImplemented("could not bind parameters: only supported with %s", OptionStatementCall)
	}
	s.releaseParams()
	s.params = []arrow.RecordBatch{}
	for stream.Next() {
		rec := stream.RecordBatch()
		rec.Retain()
		s.params = append(s.params, rec)
	}
	if err := stream.Err(); err != nil {
		s.releaseParams()
		return s.ErrorHelper.WrapInvalidArgument(err, "could not bind parameter stream")
	}
	return nil
}

func (s *statementImpl) Prepare(context.Context) error {
	if s.collection != nil || s.call != nil {
		return nil
	}
	_, _, _, err := s.resolve()
	return err
}

var _ adbc.StatementExecuteSchema = (*statementImpl)(nil)
