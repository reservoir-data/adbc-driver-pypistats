# Copyright (c) 2026 Edgar Ramírez-Mondragón
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#         http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import adbc_driver_manager.dbapi


def test_package() -> None:
    # The driver has nothing to connect to, so loading it is enough to verify
    # the package: no network access or credentials are needed.
    with adbc_driver_manager.dbapi.connect(driver="pypistats") as conn:
        tables = {
            table["table_name"]
            for catalog in conn.adbc_get_objects(depth="tables").read_all().to_pylist()
            for schema in catalog["catalog_db_schemas"]
            for table in schema["db_schema_tables"]
        }
    assert tables == {"python_minor", "system"}
