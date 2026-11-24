<!--
  Copyright (c) 2026 Edgar Ramírez-Mondragón

  Licensed under the Apache License, Version 2.0 (the "License");
  you may not use this file except in compliance with the License.
  You may obtain a copy of the License at

          http://www.apache.org/licenses/LICENSE-2.0

  Unless required by applicable law or agreed to in writing, software
  distributed under the License is distributed on an "AS IS" BASIS,
  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  See the License for the specific language governing permissions and
  limitations under the License.
-->

# How to Contribute

## Reporting Issues and Making Feature Requests

Please file issues and feature requests on the GitHub issue tracker:
https://github.com/edgarrmondragon/adbc-driver-pypistats/issues

Potential security vulnerabilities should be reported privately through
[GitHub's vulnerability
reporting](https://github.com/edgarrmondragon/adbc-driver-pypistats/security/advisories/new)
instead of the public issue tracker.

## Development Environment

You will need [Go](https://go.dev/) and [pixi](https://pixi.sh/). The driver
lives in the `go/` subdirectory.

## Build and Test

For basic development, the driver can be built and tested like any Go project.
From the `go/` subdirectory:

```shell
go build ./...
go test -tags assert -v ./...
```

This will not produce a shared library, however; that requires the full build
script. From `go/`:

```shell
pixi run make
pixi run adbc-make check
```

The first command writes `go/build/libadbc_driver_pypistats.{dylib,so,dll}`.
The second verifies the exported symbols and runtime dependencies of that
library.

To exercise the library against the real pypistats.org API through the ADBC
driver manager, run from the repository root:

```shell
uv run python/smoke.py
```

## Development Standards

- Ensure the Apache license header is at the top of all files.
- Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/)
  for commit and pull request titles. The component should be `go` if it
  affects the Go driver, or it can be omitted for general maintenance.
  Example titles:

  - `feat(go): support a new table`
  - `chore: update action versions`
  - `fix!(go): rename an option`

  Flag breaking changes with a `!`.
- Run the static checks with [pre-commit](https://pre-commit.com/) or
  [prek](https://prek.j178.dev/): `pre-commit run --all-files`. Make sure
  all your changes are staged or committed (unstaged changes are ignored).
- Generated files (`go/pixi.toml`, `.golangci.toml` and everything under
  `.github/workflows/` except `generate.toml`) are written by
  `adbc-gen-workflow`. Edit `.github/workflows/generate.toml` and regenerate
  instead of editing them by hand:

  ```shell
  uvx --from git+https://github.com/adbc-drivers/dev adbc-gen-workflow generate $(pwd)
  ```

## Opening a Pull Request

- Review your changes and make sure no stray files are included.
- Check if there is an existing issue. If not, please file one, unless the
  change is trivial.
- Make sure the bottom of the description has `Closes #NNN`, `Fixes #NNN`, or
  similar, so that the issue will be linked to your pull request.
