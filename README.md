# ADBC driver for pypistats.org

A read-only [ADBC](https://arrow.apache.org/adbc/) driver, written in Go on
[driverbase-go](https://github.com/adbc-drivers/driverbase-go), that exposes the
[pypistats.org API](https://pypistats.org/api/) as Arrow record streams.

## Tables

The statement query is a table name (`SELECT * FROM <table>` also works),
optionally filtered by package:

```sql
SELECT * FROM system WHERE package = 'urllib3'
SELECT * FROM system WHERE package IN ('urllib3', 'requests')
```

Only a single `package = '...'` or `package IN (...)` predicate is supported.
Any other SQL (other columns, `AND`/`OR`, `LIMIT`, ...) is rejected with
`NOT_IMPLEMENTED` rather than ignored, so results never silently differ from
what the query asks for. Without a predicate, the `pypistats.query.packages`
option is used as the default scope; a predicate always overrides it.

| Table          | Endpoint                               |
|----------------|----------------------------------------|
| `python_minor` | `/api/packages/<pkg>/python_minor`     |
| `system`       | `/api/packages/<pkg>/system`           |

Both share one schema:

```
package:   string  not null
category:  string  not null  # minor version or OS; "null" when pypistats reports it unknown
date:      date32  not null
downloads: uint32  not null
```

The driver fetches packages concurrently and emits one record batch per
package, in the order the packages are listed.

## Options

| Key                               | Default                      | Scope               |
|-----------------------------------|------------------------------|---------------------|
| `pypistats.query.packages`         | (none; default scope for queries without a `WHERE package ...`) | database, statement |
| `pypistats.max_concurrency`  | (auto: CPU count, clamped to 2–8) | database       |
| `pypistats.max_retries`      | `3` (HTTP 429/5xx, honors `Retry-After`) | database |
| `pypistats.request_timeout`  | `30` (seconds)               | database            |
| `pypistats.base_url`         | `https://pypistats.org/api`  | database            |

`packages` is a comma-separated string, because profile values can't be TOML
arrays. See [`profiles/pypistats.toml`](profiles/pypistats.toml).

## Build & use

```sh
make build   # build/libadbc_driver_pypistats.{dylib,so,dll}
make test    # Go tests against a fake server
make smoke   # real API, via the Python driver manager + profile
```

```python
from adbc_driver_manager import dbapi

# with ADBC_PROFILE_PATH=./profiles
with dbapi.connect(driver="build/libadbc_driver_pypistats.dylib",
                   uri="profile://pypistats") as conn, conn.cursor() as cur:
    cur.execute("system")
    print(cur.fetch_arrow_table())
```

`go/pkg/` holds the cgo C-ABI export layer. It was copied from the
adbc-drivers MySQL driver (which generates it from `_tmpl/driver.go.tmpl`) and
renamed with the `PyPIStats` prefix.

## Use in DuckDB

You can use the [ADBC extension][duckdb-adbc-ext] to read PyPI stats data:

```sql
INSTALL adbc FROM community;

LOAD adbc;

CREATE MACRO read_pypistats(query) AS TABLE
SELECT * FROM read_adbc('profile:///path/to/pypistats.toml', query);

SELECT * FROM read_pypistats('SELECT * FROM system WHERE package IN (''pyarrow'', ''nanoarrow'')');
```

Using `CREATE MACRO` lets you work around the fact that `ATTACH ... (TYPE adbc)`
can't push `WHERE package` down, so it fails with "no packages selected" unless
the configured profile sets `"pypistats.query.packages"`.

## pypistats usage notes

The API keeps 180 days of data, updates once a day, and rate-limits by IP. Don't
use it to bulk-download history for many packages; use the BigQuery
`pypi` datasets for that.

[duckdb-adbc-ext]: https://duckdb.org/community_extensions/extensions/adbc
