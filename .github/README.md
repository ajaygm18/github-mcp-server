# github-mcp-server — Hermes fork

A fork of [github/github-mcp-server](https://github.com/github/github-mcp-server) (MIT),
modified to be self-hostable and to close three gaps that make the hosted endpoint
awkward to use as a coding harness.

> Upstream's full documentation is unmodified and lives in [`README.md`](../README.md).
> This page covers only what is different here.

## Why this fork exists

The hosted endpoint at `api.githubcopilot.com/mcp/` can write code but cannot watch it
run, and every file change is a whole-file rewrite. Three changes address that.

### 1. The `actions` toolset, enabled

No code change was needed. `pkg/github/actions.go` already registers `actions_list`,
`actions_get`, `actions_run_trigger` and `actions_get_job_logs` — the `actions` toolset
simply isn't marked `Default`, and GitHub doesn't enable it on the hosted endpoint.

Self-hosting with `--toolsets=default,actions` unlocks all four. `actions_get_job_logs`
reads raw job logs, including failed steps, which removes the need for workflows to
commit a report file back into the repo just so an agent can read the result.

### 2. `edit_file` — substring edits instead of whole-file rewrites

A new tool in `pkg/github/edit_file.go`, registered in the default `repos` toolset.

| Behaviour | Detail |
| --- | --- |
| Occurrence validation | `old_string` must match exactly once; zero matches and ambiguous matches are hard errors with actionable messages |
| `replace_all` | For intentional find-and-replace across a file |
| Sequential edits | Each edit applies to the result of the previous one |
| Deletion | Empty `new_string` |
| `dry_run` | Returns the diff without committing |
| Response | Trimmed unified diff and byte counts, not the file body |

Blob SHAs are resolved server-side, so callers never fetch a file just to write to it.

### 3. A static analysis layer

[`.github/workflows/hermes-static.yml`](workflows/hermes-static.yml) runs `go build`,
`go vet`, a `gofmt` check and `go test ./pkg/github/...` on every push to `main`, on
pull requests, and on demand. Path-filtered to `**.go`, `go.mod` and `go.sum`.

## Hosting

See **[HEROKU.md](../HEROKU.md)** for the full walkthrough. The short version:

```bash
heroku create your-app-name
heroku stack:set container -a your-app-name
git push heroku main
heroku config:set -a your-app-name \
  MCP_GATEWAY_KEY="$(openssl rand -hex 32)" \
  MCP_GATEWAY_GITHUB_TOKEN="ghp_your_token"
```

Then connect `https://your-app-name.herokuapp.com/?key=<MCP_GATEWAY_KEY>`.

### The gateway, and why the secret is mandatory

Upstream's HTTP mode is multi-tenant: it holds no credential and expects every request
to carry `Authorization: Bearer <token>`. MCP clients that can only be given a URL
therefore get rejected on every call.

`pkg/http/gateway.go` adds an opt-in single-tenant mode that injects a server-side token
instead. Because that makes the URL equivalent to write access on the token owner's
account, a shared secret is required rather than optional — **the server refuses to start**
if `MCP_GATEWAY_GITHUB_TOKEN` is set without an `MCP_GATEWAY_KEY` of at least 32
characters.

Leave `MCP_GATEWAY_GITHUB_TOKEN` unset and none of this activates; upstream per-request
bearer auth applies unchanged.

| Variable | Required | Purpose |
| --- | --- | --- |
| `MCP_GATEWAY_GITHUB_TOKEN` | No | GitHub token injected into authorized requests. Unset disables the gateway. |
| `MCP_GATEWAY_KEY` | When the token is set | Shared secret, ≥32 chars. Sent as `X-MCP-Key`, `?key=`, or a bearer token. |

## Files changed from upstream

| Path | Change |
| --- | --- |
| `pkg/github/edit_file.go` | New — the `edit_file` tool |
| `pkg/github/tools.go` | Registers `EditFile(t)` |
| `pkg/http/gateway.go` | New — gated static-token gateway |
| `pkg/http/server.go` | Registers the gateway ahead of `ExtractUserToken` |
| `Dockerfile.heroku` | New — no Node/UI stage, no BuildKit secrets, shell for `$PORT` |
| `heroku.yml` | New — container build and run command |
| `.github/workflows/hermes-static.yml` | New — build, vet, fmt, test |
| `HERMES-FORK.md`, `HEROKU.md` | New — documentation |

Upstream's `README.md` and `Dockerfile` are untouched.

## Status

The Go additions have not yet been compiled. Fork Actions are disabled by default, so
the first real build is either `go build ./...` locally or the Heroku push, which fails
loudly with the compiler error if something is wrong.

## License

MIT, inherited from upstream. See [LICENSE](../LICENSE).
