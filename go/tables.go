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
	"slices"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
)

// table describes one pypistats endpoint exposed as a virtual table.
type table struct {
	// Name is the table name, used as the statement query text and in GetObjects.
	Name string
	// Endpoint is the path segment after /api/packages/<package>/.
	Endpoint string
	// Remarks is surfaced through GetObjects.
	Remarks string
	// PrimaryKey lists the columns surfaced as the table's PRIMARY KEY
	// constraint in GetObjects.
	PrimaryKey []string
}

var tables = []table{
	{
		Name:       "python_minor",
		Endpoint:   "python_minor",
		Remarks:    "Daily downloads per package, broken down by Python minor version (e.g. 3.12). Mirrors excluded.",
		PrimaryKey: []string{"package", "category", "date"},
	},
	{
		Name:       "system",
		Endpoint:   "system",
		Remarks:    "Daily downloads per package, broken down by operating system (darwin, linux, windows, other). Mirrors excluded.",
		PrimaryKey: []string{"package", "category", "date"},
	},
}

// categorySchema is shared by every "category"-style endpoint.
var categorySchema = arrow.NewSchema([]arrow.Field{
	{Name: "package", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "category", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "date", Type: arrow.FixedWidthTypes.Date32, Nullable: false},
	{Name: "downloads", Type: arrow.PrimitiveTypes.Uint32, Nullable: false},
}, nil)

func lookupTable(name string) (table, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	i := slices.IndexFunc(tables, func(t table) bool { return t.Name == name })
	if i < 0 {
		return table{}, false
	}
	return tables[i], true
}

func tableNames() []string {
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = t.Name
	}
	return names
}
