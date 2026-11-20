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
}

var tables = []table{
	{
		Name:     "python_minor",
		Endpoint: "python_minor",
		Remarks:  "Daily downloads per package, broken down by Python minor version (e.g. 3.12). Mirrors excluded.",
	},
	{
		Name:     "system",
		Endpoint: "system",
		Remarks:  "Daily downloads per package, broken down by operating system (darwin, linux, windows, other). Mirrors excluded.",
	},
}

// categorySchema is shared by every "category"-style endpoint.
var categorySchema = arrow.NewSchema([]arrow.Field{
	{Name: "package", Type: arrow.BinaryTypes.String, Nullable: false},
	{Name: "category", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "date", Type: arrow.FixedWidthTypes.Date32, Nullable: false},
	{Name: "downloads", Type: arrow.PrimitiveTypes.Int64, Nullable: false},
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
