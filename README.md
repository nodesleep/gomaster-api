# GoMaster API

A small Judge0-style API for running single-file Go programs locally on Linux. Submit source and stdin, poll for results, and optionally judge stdout against expected output. No Docker or third-party Go dependencies are required. This is an independent API, not a drop-in implementation of Judge0's protocol.

## Requirements and execution model

- Linux and Go 1.24 or later. `GO_BINARY` must name the actual Go compiler (`go version` must work).
- **Use only with trusted code.** Programs run as the API's operating-system user and can access its files, network, and other processes. JWTs control access to the API, not what submitted programs can do. Use a dedicated, unprivileged account on a disposable machine without sensitive files. Do not expose this local runner as a public arbitrary-code execution service.
- Only one Go source file, `package main`, with standard-library imports is supported. Module downloads and CGO are disabled.
- Compilation has a 30-second deadline; execution has a 3-second deadline. Stdout and stderr share a 128 KiB cap per phase. Processes run in separate process groups, which are killed after completion. Programs can escape process groups; this is cleanup, not isolation.
- `GOMAXPROCS=1` and `GOMEMLIMIT=128MiB` are passed to programs. These are runtime hints, **not hard CPU or memory limits**. Disk usage and process counts are not capped.
- There are 2 workers by default, a queue of 32, and at most 1,000 retained submissions. Results expire after one hour and are held in memory; restarting the API loses them. There is no persistence, user registration, or distributed scheduling.

## Start

```bash
# In the gomaster-api checkout:
export JWT_SECRET="$(openssl rand -hex 32)"
export API_KEY="$(openssl rand -hex 32)"
export ALLOWED_ORIGINS=http://localhost:3000
# Set this if `go` on PATH is not the real compiler:
# export GO_BINARY=/path/to/go/bin/go

go build -o gomaster-api .
./gomaster-api
```

Do not commit secrets. `.env.example` lists configuration, but the application does not automatically load `.env` files. Export settings through your shell or service manager. Keep secrets stable across restarts when previously issued JWTs must remain valid. Use HTTPS through a reverse proxy for remote access, with request rate limits and appropriate access controls.

| Variable | Default | Purpose |
| --- | --- | --- |
| `JWT_SECRET` | required | HS256 signing secret, at least 32 bytes |
| `API_KEY` | required | Separate token-issuance key, at least 32 bytes |
| `LISTEN_ADDR` | `:8080` | HTTP bind address; use `127.0.0.1:8080` for local access only |
| `GO_BINARY` | `go` | Compiler executable |
| `WORKERS` | `2` | Concurrent compile/execution workers, 1–8 |
| `ALLOWED_ORIGINS` | empty | Comma-separated exact browser origins; empty denies cross-origin requests |

## Authentication

Your editor's **trusted backend** calls the token endpoint after authenticating its own user. Choose `subject` from the authenticated user ID; do not accept an arbitrary user ID from a browser. Never expose `API_KEY` or `JWT_SECRET` in frontend code. Anyone holding `API_KEY` can mint a JWT for any subject.

```bash
curl -sS http://localhost:8080/v1/auth/token \
  -H "X-API-Key: $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"subject":"editor-user-123"}'
```

Response: `{"access_token":"...","expires_in":3600,"token_type":"Bearer"}`.

Tokens expire in one hour. The API validates HS256 signatures, issuer `gomaster-api`, audience `gomaster-editor`, expiry, issue time, and subject. There is no refresh-token or revocation endpoint; request a new token through the trusted backend. Each user can read only submissions with their own JWT subject. CORS is a browser policy, not authentication; non-browser clients still need JWTs.

## Submit and retrieve

Copy the issued token into `TOKEN`:

```bash
export TOKEN='your-issued-JWT'
curl -i http://localhost:8080/v1/submissions \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"source_code":"package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"Hello, Go!\")}","stdin":"","expected_output":"Hello, Go!\n"}'
```

Returns HTTP **202**, a `Location` header, and a JSON submission containing `id`, `status`, `stdout`, `stderr`, `compile_output`, `time_ms`, and `created_at`. Poll using the ID:

```bash
curl -sS "http://localhost:8080/v1/submissions/REPLACE_WITH_ID" \
  -H "Authorization: Bearer $TOKEN"
```

Example completed response:

```json
{
  "id": "d8f74dc8406d039d3a57ef93aa14b9c45",
  "status": "accepted",
  "stdout": "Hello, Go!\n",
  "stderr": "",
  "compile_output": "",
  "exit_code": 0,
  "time_ms": 2,
  "created_at": "2026-10-10T16:00:00Z"
}
```

`source_code` is required. `stdin` defaults to empty. Omit `expected_output` to run without judging. When supplied, stdout must match it exactly, including whitespace and newlines. Each input field is limited to 64 KiB, and the JSON request to 256 KiB. Unknown fields and additional JSON values are rejected. Source and stdin are not returned in results.

| Status | Meaning |
| --- | --- |
| `queued`, `processing` | Keep polling |
| `accepted` | Exited successfully; expected output matched if provided |
| `wrong_answer` | Successful execution but stdout did not match |
| `compile_error` | Build failed or exceeded its deadline; see `compile_output` |
| `runtime_error` | Program failed; see `stderr` and `exit_code` |
| `time_limit_exceeded` | Execution exceeded 3 seconds |
| `output_limit_exceeded` | Compile or execution output exceeded 128 KiB; output is truncated |
| `internal_error` | Runner setup failed or server shutdown cancelled work |

`time_ms` measures execution, excluding compilation and queue time. `exit_code` is present only after execution; `-1` usually means signal termination. `compile_output` contains compilation diagnostics. Limits are server-controlled, not caller-configurable.

## Web editor example

Obtain `token` from your own authenticated backend, then call the API from your editor. Configure the editor's exact origin in `ALLOWED_ORIGINS`.

```js
async function executeGo(sourceCode, stdin, token, signal) {
  const base = 'http://localhost:8080'; // configure your HTTPS API URL in deployment
  const headers = { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' };
  async function check(response) {
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
    return data;
  }
  const job = await check(await fetch(`${base}/v1/submissions`, {
    method: 'POST', headers,
    body: JSON.stringify({ source_code: sourceCode, stdin }), signal,
  }));
  // Bound polling independently; aborting HTTP does not cancel queued execution.
  const deadline = Date.now() + 120_000;
  while (Date.now() < deadline) {
    await new Promise(resolve => setTimeout(resolve, 500));
    const result = await check(await fetch(`${base}/v1/submissions/${job.id}`, { headers, signal }));
    if (!['queued', 'processing'].includes(result.status)) return result;
  }
  throw new Error('Polling timed out; the submission may still finish');
}
```

HTTP errors have shape `{"error":"message"}`: 400 invalid input, 401 missing/invalid authentication, 403 disallowed browser origin, 404 unknown endpoint or unavailable/other-user submission, 503 queue or retention capacity reached. `GET /health` is unauthenticated and returns `{"status":"ok"}`. Startup checks the compiler; health does not execute a program.

## Development and tests

```bash
go test -race ./...
# Include real compilation, stdin, runtime-error, timeout, output-limit,
# expected-output, and environment-inheritance checks:
TEST_GO_BINARY="$(command -v go)" go test -race -v ./...
go vet ./...
```

Compiler integration tests are skipped unless `TEST_GO_BINARY` is set. On machines where the default build-cache location is unwritable, export `GOCACHE=/tmp/gomaster-build-cache`. The runner uses `/tmp/gomaster-go-cache` for its own compilation cache and removes individual submission directories after execution.
