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

// Experimental: metadata collections (apache/arrow-adbc#4694) and vendor
// calls (apache/arrow-adbc#4771). Neither is standardized yet; both ride on
// existing statement APIs, so no new ABI is needed:
//
//	SetOption("adbc.metadata.collection", "calls")  -> ExecuteQuery lists calls
//	SetOption("adbc.statement.call", "pypistats.recent")
//	GetParameterSchema  -> the call's inputs
//	ExecuteSchema       -> its output, without running it
//	Bind/BindStream     -> one invocation per bound row
//	ExecuteQuery        -> one output row per bound row, in order

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/adbc-drivers/driverbase-go/driverbase"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

const (
	// OptionMetadataCollection selects a metadata collection (arrow-adbc#4694).
	OptionMetadataCollection = "adbc.metadata.collection"
	// OptionStatementCall selects a vendor call (proposed in arrow-adbc#4771).
	OptionStatementCall = "adbc.statement.call"

	// collectionCalls is the hypothetical standard collection from #4771.
	collectionMeta  = "meta"
	collectionCalls = "calls"

	// filterRemarksKey documents a filter/parameter in its field metadata.
	filterRemarksKey = "ADBC:metadata:filter:remarks"
)

// collection is a metadata collection with no filters.
type collection struct {
	Name    string
	Remarks string
	Schema  *arrow.Schema
}

var collections = []collection{
	{
		Name:    collectionMeta,
		Remarks: "The available metadata collections.",
		Schema: arrow.NewSchema([]arrow.Field{
			{Name: "collection_name", Type: arrow.BinaryTypes.String, Nullable: false},
			{Name: "collection_remarks", Type: arrow.BinaryTypes.String, Nullable: true},
		}, nil),
	},
	{
		Name:    collectionCalls,
		Remarks: "Vendor operations invocable via " + OptionStatementCall + ". Inputs via GetParameterSchema, outputs via ExecuteSchema.",
		Schema: arrow.NewSchema([]arrow.Field{
			{Name: "call_name", Type: arrow.BinaryTypes.String, Nullable: false},
			{Name: "call_remarks", Type: arrow.BinaryTypes.String, Nullable: true},
		}, nil),
	},
}

func lookupCollection(name string) (collection, bool) {
	i := slices.IndexFunc(collections, func(c collection) bool { return c.Name == name })
	if i < 0 {
		return collection{}, false
	}
	return collections[i], true
}

// rows returns (name, remarks) pairs; both collections share that shape.
func (c collection) rows() [][2]string {
	var out [][2]string
	switch c.Name {
	case collectionMeta:
		for _, x := range collections {
			out = append(out, [2]string{x.Name, x.Remarks})
		}
	case collectionCalls:
		for _, x := range calls {
			out = append(out, [2]string{x.Name, x.Remarks})
		}
	}
	return out
}

func (c collection) read(alloc memory.Allocator) (array.RecordReader, error) {
	b := array.NewRecordBuilder(alloc, c.Schema)
	defer b.Release()
	for _, row := range c.rows() {
		b.Field(0).(*array.StringBuilder).Append(row[0])
		b.Field(1).(*array.StringBuilder).Append(row[1])
	}
	rec := b.NewRecordBatch()
	defer rec.Release()
	return array.NewRecordReader(c.Schema, []arrow.RecordBatch{rec})
}

// call is a vendor operation that doesn't fit the table model. Each bound
// parameter row is one invocation, producing one result row.
type call struct {
	Name     string
	Remarks  string
	Endpoint string
	Params   *arrow.Schema
	Result   *arrow.Schema
}

var calls = []call{
	{
		Name:     "pypistats.recent",
		Remarks:  "Download totals for the last day, week and month. Mirrors excluded.",
		Endpoint: "recent",
		Params: arrow.NewSchema([]arrow.Field{
			{
				Name: "package", Type: arrow.BinaryTypes.String, Nullable: false,
				Metadata: arrow.NewMetadata([]string{filterRemarksKey}, []string{"PyPI package name."}),
			},
		}, nil),
		Result: arrow.NewSchema([]arrow.Field{
			{Name: "package", Type: arrow.BinaryTypes.String, Nullable: false},
			{Name: "last_day", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
			{Name: "last_week", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
			{Name: "last_month", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
		}, nil),
	},
}

func lookupCall(name string) (call, bool) {
	i := slices.IndexFunc(calls, func(c call) bool { return c.Name == name })
	if i < 0 {
		return call{}, false
	}
	return calls[i], true
}

func callNames() []string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.Name
	}
	return names
}

func collectionNames() []string {
	names := make([]string, len(collections))
	for i, c := range collections {
		names[i] = c.Name
	}
	return names
}

// stringValues is satisfied by utf8, large_utf8 and utf8_view arrays.
type stringValues interface {
	arrow.Array
	Value(int) string
}

// packageArgs extracts the "package" argument from every bound row, matching
// parameters by name as #4694 prescribes for filters.
func (c call) packageArgs(errs *driverbase.ErrorHelper, params []arrow.RecordBatch) ([]string, error) {
	var out []string
	for _, rec := range params {
		var col stringValues
		for i, f := range rec.Schema().Fields() {
			if !c.Params.HasField(f.Name) {
				return nil, errs.InvalidArgument("could not bind %s: unknown parameter %q, expected %v", c.Name, f.Name, c.Params)
			}
			s, ok := rec.Column(i).(stringValues)
			if !ok {
				return nil, errs.InvalidArgument("could not bind %s: parameter %q must be a string, got %s", c.Name, f.Name, f.Type)
			}
			col = s
		}
		if col == nil {
			return nil, errs.InvalidArgument("could not bind %s: missing required parameter %q", c.Name, "package")
		}
		for i := range col.Len() {
			if col.IsNull(i) {
				return nil, errs.InvalidArgument("could not bind %s: parameter %q is required but row %d is null", c.Name, "package", len(out))
			}
			out = append(out, strings.TrimSpace(col.Value(i)))
		}
	}
	return out, nil
}

type recentResponse struct {
	Data struct {
		LastDay   int64 `json:"last_day"`
		LastWeek  int64 `json:"last_week"`
		LastMonth int64 `json:"last_month"`
	} `json:"data"`
}

// invoke runs the call once per package, concurrently, and returns the
// results in input order.
func (c call) invoke(ctx context.Context, conn *connectionImpl, errs *driverbase.ErrorHelper, packages []string) (array.RecordReader, error) {
	results := make([]recentResponse, len(packages))
	errsOut := make([]error, len(packages))
	sem := make(chan struct{}, max(1, conn.cfg.effectiveMaxConcurrency()))
	var wg sync.WaitGroup
	for i, pkg := range packages {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := conn.client.fetchJSON(ctx, pkg, c.Endpoint, &results[i]); err != nil {
				errsOut[i] = wrapFetchError(errs, err, pkg, "call "+c.Name, c.Endpoint)
			}
		})
	}
	wg.Wait()
	for _, err := range errsOut {
		if err != nil {
			return nil, err
		}
	}

	b := array.NewRecordBuilder(conn.Alloc, c.Result)
	defer b.Release()
	b.Reserve(len(packages))
	for i, pkg := range packages {
		d := results[i].Data
		b.Field(0).(*array.StringBuilder).Append(pkg)
		b.Field(1).(*array.Int64Builder).Append(d.LastDay)
		b.Field(2).(*array.Int64Builder).Append(d.LastWeek)
		b.Field(3).(*array.Int64Builder).Append(d.LastMonth)
	}
	rec := b.NewRecordBatch()
	defer rec.Release()
	return array.NewRecordReader(c.Result, []arrow.RecordBatch{rec})
}
