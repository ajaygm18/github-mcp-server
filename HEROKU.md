# Hosting this fork on Heroku

## Why any of this is necessary

The upstream HTTP server is multi-tenant. It holds no credential of its own and
expects every request to carry `Authorization: Bearer <github-token>`. That is
the right design for a shared deployment, but it does not work with MCP clients
that can only be handed a URL — every call would come back unauthorized.

This fork adds an opt-in single-tenant gateway (`pkg/http/gateway.go`). When
enabled it injects a server-side GitHub token into each request, as well as providing server-side `E2B_API_KEY` authorization for E2B Cloud Sandbox execution.

Because that turns the URL into write access to your account and cloud execution resources, a shared secret is **mandatory**, not optional: the server refuses to start if the token is set without a key of at least 32 characters.

If you leave `MCP_GATEWAY_GITHUB_TOKEN` unset, none of this activates and upstream
per-request bearer authentication applies unchanged.

## Deploy

Container stack, using `heroku.yml` and `Dockerfile.heroku`:

```bash
heroku create your-app-name
heroku stack:set container -a your-app-name
git remote add heroku https://git.heroku.com/your-app-name.git
git push heroku main
```

## Configure

Generate a gateway key and set environment variables:

```bash
heroku config:set -a your-app-name \
  MCP_GATEWAY_KEY="$(openssl rand -hex 32)" \
  E2B_API_KEY="e2b_21989f72c78e8d8b6d660db36168105116fc45ad" \
  MCP_GATEWAY_GITHUB_TOKEN="ghp_your_personal_access_token"
```

Retrieve the key you just generated:

```bash
heroku config:get MCP_GATEWAY_KEY -a your-app-name
```

## Connect

The MCP endpoint is mounted at the root path. Clients that cannot set custom
headers (such as Notion AI) pass the secret in the query string:

```
https://your-app-name.herokuapp.com/?key=<MCP_GATEWAY_KEY>
```

Clients that can set headers should prefer `X-MCP-Key` instead, keeping the
secret out of logs and referrers.

Useful route variants:

| Route | Effect |
| --- | --- |
| `/` | Full configured toolset (GitHub + E2B Cloud Desktop & Interpreter) |
| `/readonly` | Read-only tools only |
| `/x/e2b` | E2B Cloud Sandbox & Desktop GUI tools only |
| `/x/actions` | GitHub Actions tools only |
| `/x/{toolset}` | A single named toolset |

## Verify

```bash
curl -s https://your-app-name.herokuapp.com/ \
  -H "X-MCP-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Look for `actions_get_job_logs` and `e2b_run_code` in the response. Its presence is the signal that self-hosting achieved the goal of executing GitHub operations and cloud sandboxed code runner directly.

## Operational notes

- **Token & API Key scope.** The injected token and E2B API Key are used for requests.
- **Eco dynos sleep.** The first request after idling pays a cold start. This
  shows up as a slow first tool call, not an error.
- **Rotation.** `heroku config:set MCP_GATEWAY_KEY=...` restarts the dyno and
  immediately invalidates the old URL.
- **Request timeout.** Heroku's router caps requests at 30 seconds. Streamable
  HTTP is used in stateless mode, so ordinary tool calls are unaffected.
