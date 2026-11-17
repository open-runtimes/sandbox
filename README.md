# open-runtimes/sandbox

The reference implementation of the **sandbox contract**: exec, background
processes, terminals and whole-file access over HTTP, scoped to one workspace
directory.

A [sandbox](https://github.com/open-runtimes/orchestrator/blob/main/docs/sandboxes.md)
is a live, isolated workspace you drive from the outside — create one, run
commands in it, read and write its files, tear it down. The orchestrator
creates, routes, and reaps sandboxes; it does not sit in the middle of your
commands. Exec and files are served by the sandbox *image*, at the sandbox's own
URL. That is what this server is.

Any image answering these routes on the pool's port is a valid sandbox
image. This one is the default.

## The contract

### `POST /execute`

```bash
curl -X POST http://localhost:3000/execute \
  -H "Content-Type: application/json" \
  -d '{"command": "python -c \"print(2+2)\"", "timeoutSeconds": 30}'
```

```json
{"exitCode": 0, "stdout": "4\n", "stderr": "", "durationMillis": 41, "truncated": false}
```

`command` runs through `$SANDBOX_SHELL -c` with the workspace as its working
directory, in its own process group so a timeout takes the whole process tree
with it.

- **Serialized.** One sandbox is one seat — a second `/execute` against a busy
  sandbox is `409`, because concurrent execs would race on a shared filesystem.
- **`timeoutSeconds`** defaults to 60. `0` means run until it exits. A command
  killed by the timeout reports `exitCode: 124`, the `timeout(1)` convention.
- **Output is capped** at 1 MiB per stream, past which `truncated` is `true`.
  Beyond that, write to a file and read it back through `/files`, or stream it.
- **`"stream": true`** answers with `application/x-ndjson` instead, one event
  per line as output arrives, then exactly one `exit` event. Nothing is capped.
  A command that cannot start reports it as `stderr` and an `exit`, since the
  `200` is already sent.

```json
{"type": "stdout", "data": "building...\n"}
{"type": "stderr", "data": "warning: ...\n"}
{"type": "exit", "exitCode": 0, "durationMillis": 1840}
```

Commands inherit the server's environment, so variables set on the sandbox's
container reach every command and process.

### `/processes`

Background processes: dev servers, watchers, anything that outlives a request.
They do not take the `/execute` seat, and run alongside each other.

| Route | |
| --- | --- |
| `POST /processes` | Start one: `{"command", "pty", "cols", "rows"}`. `201` with the process. |
| `GET /processes` | `{"processes": [...]}`, running and exited. |
| `GET /processes/{id}` | One process. |
| `GET /processes/{id}/output` | Its output from `?offset`, see below. |
| `POST /processes/{id}/input` | The raw body is written to its stdin (or typed into its terminal). `204`; `409` once it has exited. |
| `PUT /processes/{id}/size` | Resize its terminal: `{"cols", "rows"}`. `204`. |
| `DELETE /processes/{id}` | Kill its process group if running, then forget it. `204`. |

A process is `{"id", "command", "pty", "running", "exitCode", "startedAt",
"endedAt", "cols", "rows"}`. One killed by a signal exits with 128 plus the
signal number. A sandbox keeps 32 processes; exited ones are dropped oldest
first to make room, and starting a 33rd running one is `409`.

stdout and stderr are kept as one output, in the order written, and the last
1 MiB of it is retained. `GET /output` addresses it by absolute byte offset:

- By default it is a long-poll: `{"data", "offset", "nextOffset", "running",
  "exitCode"}` with `data` base64-encoded, waiting up to `?wait` seconds (max
  30) when there is nothing at `?offset` yet. Read again from `nextOffset`. An
  offset older than the retained tail comes back with a later `offset` than
  asked for, which is how a reader sees the gap.
- `?follow=true` streams the raw bytes instead, from `?offset` (echoed as
  `X-Output-Offset`) until the process exits or the client disconnects.

### Terminals

A terminal is a process started with `"pty": true`: it runs on a
pseudo-terminal of `cols` by `rows` (default 80 by 24, at most 1000), as its
own session, so job control, `^C` and resize work as they would over SSH.
Without a `command` it is an interactive `$SANDBOX_SHELL`. Keystrokes go to
`/input`, the screen comes back from `/output`, and `/size` delivers
`SIGWINCH`. `TERM` defaults to `xterm-256color`. Terminals need Linux.

```bash
curl -X POST localhost:3000/processes -d '{"pty": true, "cols": 120, "rows": 40}'   # {"id": "1", ...}
curl -X POST localhost:3000/processes/1/input --data-binary $'ls -la\n'
curl "localhost:3000/processes/1/output?offset=0&wait=5"
```

### `GET|PUT|DELETE /files/{path}`

```bash
curl -X PUT http://localhost:3000/files/main.py --data-binary @main.py   # 204
curl        http://localhost:3000/files/out.json                        # the bytes
curl        http://localhost:3000/files/                                # JSON listing
curl -X DELETE http://localhost:3000/files/out.json                     # 204
```

Whole-file read, write, and remove, relative to the workspace. `PUT` creates
parent directories and truncates an existing file. `GET` on a directory lists it
as `{"entries": [{"name", "size", "isDir"}]}`. `DELETE` removes a directory
tree. Absolute paths and `..` are `400`; a missing path is `404`.

`/files` is a convenience, not a security boundary — it is scoped to the
workspace, but `/execute` runs with the container's full filesystem reach
either way. The boundary is the container and its
[isolation tier](https://github.com/open-runtimes/orchestrator/blob/main/docs/operations.md#isolation-tiers).

For anything bulkier than a handful of files, use the orchestrator's
`artifacts` at create time — they are materialized into the workspace before the
sandbox reports ready.

### `GET /healthz`

`{"status": "ok"}` once the server is listening. This is the pool's probe
subject.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `SANDBOX_WORKSPACE` | `/workspace` | working directory for exec, root for `/files` |
| `SANDBOX_PORT` | `3000` | listen port |
| `SANDBOX_SHELL` | `/bin/sh` | shell commands, processes and terminals run through |

## Running it

```bash
docker run -p 3000:3000 ghcr.io/open-runtimes/sandbox:latest
```

The image is Alpine plus the server — busybox is the shell `/execute` runs
commands through, and it runs as an unprivileged user with `/workspace` as its
working directory. Base images are pinned by digest.

Locally, either build it or take a binary from a
[release](https://github.com/open-runtimes/sandbox/releases) (linux and macOS,
amd64 and arm64):

```bash
SANDBOX_WORKSPACE=$(mktemp -d) go run .
docker build -t sandbox .
```

## Language images

The reference image carries a shell and nothing else. A language sandbox is this
binary dropped into a runtime image:

```dockerfile
FROM ghcr.io/open-runtimes/sandbox:latest AS contract

FROM python:3.12-slim
COPY --from=contract /usr/local/bin/sandbox /usr/local/bin/sandbox
RUN useradd -m sandbox && mkdir /workspace && chown sandbox /workspace
ENV SANDBOX_WORKSPACE=/workspace SANDBOX_PORT=3000
USER sandbox
WORKDIR /workspace
EXPOSE 3000
ENTRYPOINT ["/usr/local/bin/sandbox"]
```

Nothing about the contract is language-specific — the server only ever shells
out, so whatever is on `PATH` in your image is what `/execute` can run.

## Development

```bash
go test -race ./...
go vet ./...
golangci-lint run
```

CI lints, tests, builds the image, and smoke-tests the contract inside it.
Pushing a `v*` tag publishes a multi-arch image to
`ghcr.io/open-runtimes/sandbox` and attaches the binaries to a GitHub release.
