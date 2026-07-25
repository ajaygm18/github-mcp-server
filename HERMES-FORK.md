# Hermes fork of github-mcp-server

Fork of [github/github-mcp-server](https://github.com/github/github-mcp-server) (MIT).

This fork exists to close gaps that appear when an AI agent uses the **hosted**
GitHub MCP endpoint (`https://api.githubcopilot.com/mcp/`) as its only means of
reading, writing and running code.

## Gap 1 — no Actions tools on the hosted endpoint

**Fixed by self-hosting. No code change required.**

The hosted endpoint does not expose the `actions` toolset, so an agent cannot
list workflow runs, trigger a workflow, or read job logs. Without job logs the
only way to recover CI output is to make the workflow commit a report file back
into the repository.

The tools exist in this codebase already (`pkg/github/actions.go`) and are
registered in `AllTools`. They are simply gated behind a non-default toolset:

| Tool | Purpose |
| --- | --- |
| `actions_list` | List workflows, runs, jobs, artifacts |
| `actions_get` | Get a workflow, run, or job |
| `actions_run_trigger` | Dispatch a workflow, re-run, cancel |
| `actions_get_job_logs` | **Read raw job logs, including failed-step logs** |

Enable them at startup:

```bash
github-mcp-server stdio --toolsets=default,actions
```

`actions_get_job_logs` is the important one. It turns the write-then-execute
loop from "push, wait, hope the workflow wrote a report file" into "push, read
the log".

## Gap 2 — every file write is a whole-file rewrite

**Fixed in code: new `edit_file` tool (`pkg/github/edit_file.go`).**

The GitHub contents API only supports whole-file writes. `create_or_update_file`
and `push_files` therefore require the caller to re-emit the complete contents of
a file to change one line, and `create_or_update_file` additionally requires the
caller to supply the current blob SHA.

For an agent this is the dominant cost of editing: token spend scales with file
size rather than change size, and each rewrite is an opportunity to silently drop
unrelated content.

`edit_file` performs the read-modify-write cycle server-side:

```jsonc
{
  "owner": "ajaygm18",
  "repo": "my-project",
  "path": "src/server.go",
  "branch": "main",
  "message": "fix: correct retry backoff",
  "edits": [
    { "old_string": "time.Sleep(1 * time.Second)", "new_string": "time.Sleep(backoff)" }
  ]
}
```

Behaviour:

- **Occurrence validation.** `old_string` must match exactly once. Zero matches
  and ambiguous matches are hard errors with actionable messages, rather than a
  silently wrong edit.
- **`replace_all`** for intentional find-and-replace.
- **Sequential edits.** Multiple edits apply in order, each seeing the previous
  result.
- **Deletion** by passing an empty `new_string`.
- **`dry_run`** validates and returns the diff without committing.
- **No SHA management.** The blob SHA is resolved server-side.
- **Compact response.** Returns a trimmed unified-style diff and byte counts,
  not the file body, so responses scale with the change too.

It is registered in the `repos` toolset, which is enabled by default.

## Gap 3 — no static feedback without a CI round trip

**Not fixed in code. Fixed in workflow configuration.**

An agent driving a repository remotely has no language server and no linter, so
trivial mistakes that a local editor would surface instantly are only caught by
running CI.

The practical mitigation is a fast static job that runs on every push and stays
under ten seconds, separate from the full test suite. Combined with
`actions_get_job_logs` from Gap 1, this gives quick, readable feedback:

```yaml
name: static
on: [push, workflow_dispatch]
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - run: go build ./...
      - run: go vet ./...
```

## Building and hosting

Requires Go 1.25+.

```bash
go build ./cmd/github-mcp-server

# stdio transport, for local MCP hosts
GITHUB_PERSONAL_ACCESS_TOKEN=ghp_... \
  ./github-mcp-server stdio --toolsets=default,actions
```

There is a Dockerfile in the repository root for container deployment.

To use it as a remote server that Notion or another hosted client can reach, put
it behind HTTPS with authentication in front. **Do not expose it unauthenticated**
— it holds write access to every repository the token can reach.

## Upstream

These changes are additive and do not modify existing tool behaviour, so this
fork can track upstream `main` with rebases. `edit_file` is a plausible upstream
contribution; the Actions toolset needs no change, only a startup flag.
