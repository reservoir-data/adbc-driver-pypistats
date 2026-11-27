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
	"testing"

	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (h *harness) stmt(t *testing.T) adbc.StatementWithContext {
	t.Helper()
	st, err := h.conn.NewStatement(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close(context.Background())) })
	return st
}

func readAll(t *testing.T, rdr array.RecordReader) []map[string]any {
	t.Helper()
	defer rdr.Release()
	var out []map[string]any
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range int(rec.NumRows()) {
			row := map[string]any{}
			for j, f := range rec.Schema().Fields() {
				row[f.Name] = rec.Column(j).GetOneForMarshal(i)
			}
			out = append(out, row)
		}
	}
	require.NoError(t, rdr.Err())
	return out
}

func (h *harness) packagesBatch(pkgs ...string) arrow.RecordBatch {
	b := array.NewStringBuilder(h.alloc)
	defer b.Release()
	b.AppendValues(pkgs, nil)
	col := b.NewArray()
	defer col.Release()
	schema := arrow.NewSchema([]arrow.Field{{Name: "package", Type: arrow.BinaryTypes.String}}, nil)
	return array.NewRecordBatch(schema, []arrow.Array{col}, int64(len(pkgs)))
}

func TestCollections(t *testing.T) {
	ctx := context.Background()
	h := setup(t, newFakeServer(t), nil)
	st := h.stmt(t)

	require.NoError(t, st.SetOption(ctx, OptionMetadataCollection, "meta"))
	rdr, _, err := st.ExecuteQuery(ctx)
	require.NoError(t, err)
	var names []any
	for _, r := range readAll(t, rdr) {
		names = append(names, r["collection_name"])
	}
	assert.Equal(t, []any{"meta", "calls"}, names)

	require.NoError(t, st.SetOption(ctx, OptionMetadataCollection, "calls"))
	schema, err := st.(adbc.StatementExecuteSchema).ExecuteSchema(ctx)
	require.NoError(t, err)
	assert.Equal(t, collections[1].Schema, schema)
	params, err := st.GetParameterSchema(ctx)
	require.NoError(t, err)
	assert.Zero(t, params.NumFields())
	rdr, _, err = st.ExecuteQuery(ctx)
	require.NoError(t, err)
	rows := readAll(t, rdr)
	require.Len(t, rows, 1)
	assert.Equal(t, "pypistats.recent", rows[0]["call_name"])

	err = st.SetOption(ctx, OptionMetadataCollection, "nope")
	assert.Equal(t, adbc.StatusInvalidArgument, adbcStatus(t, err))
}

func TestCallRecent(t *testing.T) {
	ctx := context.Background()
	h := setup(t, newFakeServer(t), nil)
	st := h.stmt(t)

	require.NoError(t, st.SetOption(ctx, OptionStatementCall, "pypistats.recent"))
	got, err := st.(adbc.GetSetOptionsWithContext).GetOption(ctx, OptionStatementCall)
	require.NoError(t, err)
	assert.Equal(t, "pypistats.recent", got)

	params, err := st.GetParameterSchema(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, params.NumFields())
	assert.Equal(t, "package", params.Field(0).Name)
	assert.False(t, params.Field(0).Nullable)

	schema, err := st.(adbc.StatementExecuteSchema).ExecuteSchema(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"package", "last_day", "last_week", "last_month"}, fieldNames(schema))

	_, _, err = st.ExecuteQuery(ctx)
	assert.Equal(t, adbc.StatusInvalidState, adbcStatus(t, err))

	bind(t, st, h.packagesBatch("numpy", "pip"))
	rdr, n, err := st.ExecuteQuery(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
	assert.Equal(t, []map[string]any{
		{"package": "numpy", "last_day": int64(5), "last_week": int64(7), "last_month": int64(30)},
		{"package": "pip", "last_day": int64(3), "last_week": int64(7), "last_month": int64(30)},
	}, readAll(t, rdr))

	// Switching back to a query drops the call and its bound parameters.
	require.NoError(t, st.SetSqlQuery(ctx, "system"))
	_, err = st.GetParameterSchema(ctx)
	assert.Equal(t, adbc.StatusNotImplemented, adbcStatus(t, err))
}

func TestCallBindStream(t *testing.T) {
	ctx := context.Background()
	h := setup(t, newFakeServer(t), nil)
	st := h.stmt(t)
	require.NoError(t, st.SetOption(ctx, OptionStatementCall, "pypistats.recent"))

	b1, b2 := h.packagesBatch("a"), h.packagesBatch("bb", "ccc")
	stream, err := array.NewRecordReader(b1.Schema(), []arrow.RecordBatch{b1, b2})
	require.NoError(t, err)
	b1.Release()
	b2.Release()
	require.NoError(t, st.BindStream(ctx, stream))

	rdr, _, err := st.ExecuteQuery(ctx)
	require.NoError(t, err)
	var days []any
	for _, r := range readAll(t, rdr) {
		days = append(days, r["last_day"])
	}
	assert.Equal(t, []any{int64(1), int64(2), int64(3)}, days)
}

func TestCallErrors(t *testing.T) {
	ctx := context.Background()
	h := setup(t, newFakeServer(t), nil)

	t.Run("unknown call", func(t *testing.T) {
		err := h.stmt(t).SetOption(ctx, OptionStatementCall, "pypistats.nope")
		assert.Equal(t, adbc.StatusNotFound, adbcStatus(t, err))
	})
	t.Run("bind without call", func(t *testing.T) {
		st := h.stmt(t)
		require.NoError(t, st.SetSqlQuery(ctx, "system"))
		rec := h.packagesBatch("numpy")
		defer rec.Release()
		err := st.Bind(ctx, rec)
		assert.Equal(t, adbc.StatusNotImplemented, adbcStatus(t, err))
	})
	t.Run("unknown parameter", func(t *testing.T) {
		st := h.stmt(t)
		require.NoError(t, st.SetOption(ctx, OptionStatementCall, "pypistats.recent"))
		b := array.NewStringBuilder(h.alloc)
		b.Append("numpy")
		col := b.NewArray()
		b.Release()
		rec := array.NewRecordBatch(arrow.NewSchema([]arrow.Field{{Name: "pkg", Type: arrow.BinaryTypes.String}}, nil), []arrow.Array{col}, 1)
		col.Release()
		bind(t, st, rec)
		_, _, err := st.ExecuteQuery(ctx)
		assert.Equal(t, adbc.StatusInvalidArgument, adbcStatus(t, err))
	})
	t.Run("null package", func(t *testing.T) {
		st := h.stmt(t)
		require.NoError(t, st.SetOption(ctx, OptionStatementCall, "pypistats.recent"))
		b := array.NewStringBuilder(h.alloc)
		b.AppendNull()
		col := b.NewArray()
		b.Release()
		rec := array.NewRecordBatch(arrow.NewSchema([]arrow.Field{{Name: "package", Type: arrow.BinaryTypes.String, Nullable: true}}, nil), []arrow.Array{col}, 1)
		col.Release()
		bind(t, st, rec)
		_, _, err := st.ExecuteQuery(ctx)
		assert.Equal(t, adbc.StatusInvalidArgument, adbcStatus(t, err))
	})
	t.Run("package not found", func(t *testing.T) {
		st := h.stmt(t)
		require.NoError(t, st.SetOption(ctx, OptionStatementCall, "pypistats.recent"))
		bind(t, st, h.packagesBatch("numpy", "does-not-exist"))
		_, _, err := st.ExecuteQuery(ctx)
		assert.Equal(t, adbc.StatusNotFound, adbcStatus(t, err))
	})
}

// bind binds rec and drops the caller's reference, as the cgo wrapper does.
func bind(t *testing.T, st adbc.StatementWithContext, rec arrow.RecordBatch) {
	t.Helper()
	defer rec.Release()
	require.NoError(t, st.Bind(context.Background(), rec))
}

func fieldNames(s *arrow.Schema) []string {
	out := make([]string, s.NumFields())
	for i, f := range s.Fields() {
		out[i] = f.Name
	}
	return out
}
