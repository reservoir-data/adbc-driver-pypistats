package pypistats

import (
	"context"
	"regexp"

	"github.com/adbc-drivers/driverbase-go/driverbase"
	"github.com/apache/arrow-adbc/go/adbc"
	"github.com/apache/arrow-go/v18/arrow"
)

const (
	catalogName  = "pypistats"
	dbSchemaName = "api"
	tableType    = "TABLE"
)

type connectionImpl struct {
	driverbase.ConnectionImplBase
	cfg    config
	client *client
}

var (
	_ driverbase.DbObjectsEnumerator = (*connectionImpl)(nil)
	_ driverbase.TableTypeLister     = (*connectionImpl)(nil)
	_ driverbase.AutocommitSetter    = (*connectionImpl)(nil)
)

// The driver is read-only, so transactions are meaningless. Accept either
// autocommit mode and make Commit/Rollback no-ops so the driver manager
// treats the connection as DB-API 2.0 compliant instead of warning.
func (c *connectionImpl) SetAutocommit(context.Context, bool) error { return nil }

func (c *connectionImpl) Commit(context.Context) error { return nil }

func (c *connectionImpl) Rollback(context.Context) error { return nil }

func (c *connectionImpl) NewStatement(ctx context.Context) (adbc.StatementWithContext, error) {
	return driverbase.NewStatement(&statementImpl{
		StatementImplBase: driverbase.NewStatementImplBase(&c.ConnectionImplBase, c.ErrorHelper),
		conn:              c,
		packages:          append([]string(nil), c.cfg.packages...),
	}), nil
}

func (c *connectionImpl) ListTableTypes(context.Context) ([]string, error) {
	return []string{tableType}, nil
}

func matches(pattern *string, s string) (bool, error) {
	re, err := driverbase.PatternToRegexp(pattern)
	if err != nil || re == nil {
		return err == nil, err
	}
	return re.MatchString(s), nil
}

func (c *connectionImpl) GetCatalogs(_ context.Context, catalogFilter *string) ([]string, error) {
	if ok, err := matches(catalogFilter, catalogName); !ok || err != nil {
		return nil, err
	}
	return []string{catalogName}, nil
}

func (c *connectionImpl) GetDBSchemasForCatalog(_ context.Context, catalog string, schemaFilter *string) ([]string, error) {
	if catalog != catalogName {
		return nil, nil
	}
	if ok, err := matches(schemaFilter, dbSchemaName); !ok || err != nil {
		return nil, err
	}
	return []string{dbSchemaName}, nil
}

func (c *connectionImpl) GetTablesForDBSchema(_ context.Context, catalog, schema string, tableFilter, columnFilter *string, includeColumns bool) ([]driverbase.TableInfo, error) {
	if catalog != catalogName || schema != dbSchemaName {
		return nil, nil
	}
	tableRe, err := driverbase.PatternToRegexp(tableFilter)
	if err != nil {
		return nil, err
	}
	colRe, err := driverbase.PatternToRegexp(columnFilter)
	if err != nil {
		return nil, err
	}

	var out []driverbase.TableInfo
	for _, t := range tables {
		if tableRe != nil && !tableRe.MatchString(t.Name) {
			continue
		}
		info := driverbase.TableInfo{TableName: t.Name, TableType: tableType}
		if includeColumns {
			info.TableColumns = columnInfos(categorySchema, colRe)
			info.TableConstraints = tableConstraints(t)
		}
		out = append(out, info)
	}
	return out, nil
}

func tableConstraints(t table) []driverbase.ConstraintInfo {
	if len(t.PrimaryKey) == 0 {
		return nil
	}
	return []driverbase.ConstraintInfo{{
		ConstraintName:        new(t.Name + "_pkey"),
		ConstraintType:        "PRIMARY KEY",
		ConstraintColumnNames: driverbase.RequiredList(t.PrimaryKey),
	}}
}

func columnInfos(schema *arrow.Schema, filter *regexp.Regexp) []driverbase.ColumnInfo {
	var cols []driverbase.ColumnInfo
	for i, f := range schema.Fields() {
		if filter != nil && !filter.MatchString(f.Name) {
			continue
		}
		xdbcType := int16(driverbase.ToXdbcDataType(f.Type))
		typeName := f.Type.String()
		nullable := int16(0)
		isNullable := "NO"
		if f.Nullable {
			nullable, isNullable = 1, "YES"
		}
		cols = append(cols, driverbase.ColumnInfo{
			ColumnName:      f.Name,
			OrdinalPosition: new(int32(i + 1)),
			XdbcDataType:    &xdbcType,
			XdbcSqlDataType: &xdbcType,
			XdbcTypeName:    &typeName,
			XdbcNullable:    &nullable,
			XdbcIsNullable:  &isNullable,
		})
	}
	return cols
}

func (c *connectionImpl) GetTableSchema(_ context.Context, catalog, dbSchema *string, tableName string) (*arrow.Schema, error) {
	if (catalog != nil && *catalog != catalogName) || (dbSchema != nil && *dbSchema != dbSchemaName) {
		return nil, c.ErrorHelper.NotFound("could not get schema: table %q not found", tableName)
	}
	if _, ok := lookupTable(tableName); !ok {
		return nil, c.ErrorHelper.NotFound("could not get schema: table %q not found (available: %v)", tableName, tableNames())
	}
	return categorySchema, nil
}
