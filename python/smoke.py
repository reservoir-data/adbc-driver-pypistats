"""Smoke test: load the driver via the ADBC driver manager using a profile."""

import os
import sys

from adbc_driver_manager import dbapi

driver_path = sys.argv[1]
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

    with conn.cursor() as cur:
        for table in ("python_minor", "system"):
            cur.execute(table)
            tbl = cur.fetch_arrow_table()
            print(f"\n{table}: {tbl.num_rows} rows, packages={set(tbl['package'].to_pylist())}")
            print(tbl.slice(0, 5))

        # Statement-level override of the profile's package list.
        cur.adbc_statement.set_options(**{"adbc.pypistats.packages": "pyarrow"})
        cur.execute("SELECT * FROM system")
        tbl = cur.fetch_arrow_table()
        print(f"\n{table}: {tbl.num_rows} rows, packages={set(tbl['package'].to_pylist())}")
        print("\noverride:", tbl.group_by("category").aggregate([("downloads", "sum")]))
