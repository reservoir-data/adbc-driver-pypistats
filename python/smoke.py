# Copyright (c) 2026 Edgar Ramírez-Mondragón
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#         http:#www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# /// script
# requires-python = ">=3.10"
# dependencies = ["adbc-driver-manager", "pyarrow"]
# ///
"""Smoke test: load the driver via the ADBC driver manager using a profile.

Run from anywhere with `uv run python/smoke.py [path/to/libadbc_driver_pypistats]`.
Defaults to the library built by `pixi run make` and the repo's profiles/.
"""

import os
import sys
from pathlib import Path

from adbc_driver_manager import dbapi

root = Path(__file__).resolve().parent.parent
suffix = {"darwin": "dylib", "win32": "dll"}.get(sys.platform, "so")
driver_path = sys.argv[1] if len(sys.argv) > 1 else str(root / "go" / "build" / f"libadbc_driver_pypistats.{suffix}")
os.environ.setdefault("ADBC_PROFILE_PATH", str(root / "profiles"))
os.environ["PYPISTATS_PACKAGES"] = "requests,numpy,nanoarrow"

with dbapi.connect(driver=driver_path, uri="profile://pypistats") as conn:
    print(
        "tables:",
        [
            (c["catalog_name"], s["db_schema_name"], t["table_name"])
            for c in conn.adbc_get_objects(depth="tables").read_all().to_pylist()
            for s in c["catalog_db_schemas"]
            for t in s["db_schema_tables"]
        ],
    )

    print(conn.adbc_get_table_schema("system"))
    for schema in conn.adbc_get_objects(depth="columns").read_all().to_pylist()[0]["catalog_db_schemas"]:
        for table in schema["db_schema_tables"]:
            for column in table["table_columns"]:
                print("column: ", column["column_name"], column["xdbc_type_name"])
            for constraint in table["table_constraints"]:
                print("primary key: ", constraint["constraint_column_names"])

    with conn.cursor() as cur:
        for table in ("python_minor", "system"):
            cur.execute(table)
            tbl = cur.fetch_arrow_table()
            print(f"\n{table}: {tbl.num_rows} rows, packages={set(tbl['package'].to_pylist())}")
            print(tbl.slice(0, 5))

        # Statement-level override of the profile's package list.
        cur.adbc_statement.set_options(**{"pypistats.query.packages": "pyarrow"})
        cur.execute("SELECT * FROM system")
        tbl = cur.fetch_arrow_table()
        print(f"\n{table}: {tbl.num_rows} rows, packages={set(tbl['package'].to_pylist())}")
        print("\noverride:", tbl.group_by("category").aggregate([("downloads", "sum")]))

        # Explicit predicate; takes precedence over any configured default.
        cur.execute("SELECT * FROM system WHERE package IN ('urllib3', 'requests')")
        tbl = cur.fetch_arrow_table()
        print(f"\nWHERE: {tbl.num_rows} rows, packages={set(tbl['package'].to_pylist())}")
