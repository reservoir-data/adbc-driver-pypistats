# ADBC driver for pypistats.org

A read-only [ADBC](https://arrow.apache.org/adbc/) driver, written in Go on
[driverbase-go](https://github.com/adbc-drivers/driverbase-go), that exposes the
[pypistats.org API](https://pypistats.org/api/) as Arrow record streams.

## Tables

The statement query is a table name (`SELECT * FROM <table>` also works).

| Table          | Endpoint                               |
|----------------|----------------------------------------|
| `python_minor` | `/api/packages/<pkg>/python_minor`     |
| `system`       | `/api/packages/<pkg>/system`           |

Both share one schema:

```
package:   string  not null
category:  string            # minor version or OS; null when pypistats reports "null"
date:      date32  not null
downloads: int64   not null
```

The driver fetches packages concurrently and emits one record batch per
package, in the order the packages are configured.

## Options

| Key                               | Default                      | Scope               |
|-----------------------------------|------------------------------|---------------------|
| `adbc.pypistats.packages`         | (required)                   | database, statement |
| `adbc.pypistats.max_concurrency`  | `2`                          | database            |
| `adbc.pypistats.max_retries`      | `3` (HTTP 429/5xx, honors `Retry-After`) | database |
| `adbc.pypistats.request_timeout`  | `30` (seconds)               | database            |
| `adbc.pypistats.base_url`         | `https://pypistats.org/api`  | database            |

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

## pypistats usage notes

The API keeps 180 days of data, updates once a day, and rate-limits by IP. Don't
use it to bulk-download history for many packages; use the BigQuery
`pypi` datasets for that.
