# Hosting this fork on Heroku

## Why any of this is necessary

The upstream HTTP server is multi-tenant. It holds no credential of its own and
expects every request to carry `Authorization: Bearer <github-token>`. That is
the right design for a shared deployment, but it does not work with MCP clients
that can only be handed a URL — every call would come back unauthorized.

This fork adds an opt-in single-tenant gateway (`pkg/http/gateway.go`). When
enabled it injects a server-side GitHub token into each request. Because that
turns the URL into write access to your account, a shared secret is **mandatory**,
not optional: the server refuses to start if the token is set without a key of at
least 32 characters.

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

Generate a key and set both variables:

```bash
heroku config:set -a your-app-name \
  MCP_GATEWAY_KEY="$(openssl rand -hex 32)" \
  MCP_GATEWAY_GITHUB_TOKEN="ghp_your_personal_access_token"
```

Retrieve the key you just generated:

```bash
heroku config:get MCP_GATEWAY_KEY -a your-app-name
```

## Connect

The MCP endpoint is mounted at the root path. Clients that cannot set custom
headers pass the secret in the query string:

```
https://your-app-name.herokuapp.com/?key=<MCP_GATEWAY_KEY>
```

Clients that can set headers should prefer `X-MCP-Key` instead, keeping the
secret out of logs and referrers.

Useful route variants, all inherited from upstream:

| Route | Effect |
| --- | --- |
| `/` | Full configured toolset |
| `/readonly` | Read-only tools only |
| `/x/{toolset}` | A single named toolset, e.g. `/x/actions` |

## Verify

```bash
curl -s https://your-app-name.herokuapp.com/ \
  -H "X-MCP-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```

Look for `actions_get_job_logs` in the response. Its presence is the signal that
self-hosting achieved the goal: reading CI job output directly, instead of having
workflows commit a report file back to the repo.

## Operational notes

- **Token scope.** The injected token is used for every request. Scope it to what
  you actually need; there is no per-caller narrowing once the gateway is on.
- **Eco dynos sleep.** The first request after idling pays a cold start. This
  shows up as a slow first tool call, not an error.
- **Rotation.** `heroku config:set MCP_GATEWAY_KEY=...` restarts the dyno and
  immediately invalidates the old URL.
- **Request timeout.** Heroku's router caps requests at 30 seconds. Streamable
  HTTP is used in stateless mode, so ordinary tool calls are unaffected, but do
  not expect to hold a long-lived stream open.
