# open-runtimes/sandbox

The reference implementation of the **sandbox contract**: exec and whole-file
access over HTTP, scoped to one workspace directory.

A [sandbox](https://github.com/open-runtimes/orchestrator/blob/main/docs/sandboxes.md)
is a live, isolated workspace you drive from the outside — create one, run
commands in it, read and write its files, tear it down. The orchestrator
creates, routes, and reaps sandboxes; it does not sit in the middle of your
commands. Exec and files are served by the sandbox *image*, at the sandbox's own
URL. That is what this server is.

Any image answering these three routes on the pool's port is a valid sandbox
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
  Beyond that, write to a file and read it back through `/files`.

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
| `SANDBOX_SHELL` | `/bin/sh` | shell `/execute` runs commands through |

## Running it

```bash
go run .                                      # local, needs SANDBOX_WORKSPACE set
docker build -t open-runtimes/sandbox .       # distroless, ~10 MB
docker run -p 3000:3000 open-runtimes/sandbox
```

## Language images

The reference image carries a shell and nothing else. A language sandbox is this
binary dropped into a runtime image:

```dockerfile
FROM open-runtimes/sandbox AS contract

FROM python:3.12-slim
COPY --from=contract /sandbox /sandbox
RUN useradd -m sandbox && mkdir /workspace && chown sandbox /workspace
ENV SANDBOX_WORKSPACE=/workspace SANDBOX_PORT=3000
USER sandbox
EXPOSE 3000
ENTRYPOINT ["/sandbox"]
```

Nothing about the contract is language-specific — the server only ever shells
out, so whatever is on `PATH` in your image is what `/execute` can run.

## Development

```bash
go test ./...
go vet ./...
```
