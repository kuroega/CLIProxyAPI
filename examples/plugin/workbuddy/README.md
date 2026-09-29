# WorkBuddy provider plugin (experimental)

This standalone shared-library plugin adds a `workbuddy` provider without changes to the CLIProxyAPI server. It implements international (`intl`) and China (`cn`) browser login, polling, rotating-token refresh, per-account model lists, and OpenAI Chat Completions execution (streaming and non-streaming). CLIProxyAPI's existing translators can serve other downstream protocols from this format where supported.

## Build and install

Build with Go and a working C toolchain (`CGO_ENABLED=1`) for the **same OS and architecture as the server**:

```bash
# From the repository root; use .dll on Windows, .dylib on macOS.
go build -buildmode=c-shared -o plugins/workbuddy.so ./examples/plugin/workbuddy/go
```

Enable the plugin in `config.yaml` (do not configure `workbuddy` as an `openai-compatibility` provider at the same time):

```yaml
plugins:
  enabled: true
  path:
    - /absolute/path/to/plugins/workbuddy.so
  configs:
    workbuddy:
      enabled: true
      priority: 1
```

Use the management OAuth flow: `GET /v0/management/oauth/auth-url?provider=workbuddy&realm=intl` or `realm=cn`. Open the returned `url`, then poll the management auth-status endpoint with the returned `state` (normally handled by the management UI). The plugin stores a separate credential for each account and realm. Desktop credential import is **not** supported: recent desktop builds encrypt their tokens. Existing unencrypted credential files may be placed in `auths/` with `provider`, `uid`, `realm`, `accessToken`, `refreshToken` and optional `product` (`workbuddy`, `cli`, or `vscode`). Protect that directory: it contains secrets.

## Scope and limitations

- Outbound traffic uses the host's `host.http` callbacks so the host transport, global proxy, and request logging policy apply. The callback API does **not** supply the selected auth's proxy URL; per-account proxy routing is therefore not supported in this plugin. Do not rely on it for region-specific exit IPs. Keep international and China credentials separate and set the server's global proxy accordingly.
- The model list falls back to the sibling gateway's snapshot when `/v3/config` is unavailable; the endpoint's `agents.cli.models` is used when present. New models added after plugin startup may require a restart to enter the plugin executor's initial routing list.
- Chat input supports ordinary text and tools, with minimal compatibility fixes for developer roles, `max_completion_tokens`, object `tool_choice`, and DeepSeek reasoning. It does not implement all of workbuddy2api-hub's history repair, WAF sanitization, web-agent features, billing/check-in, or server-side web tools. Tool-call streaming and cross-protocol translation should be validated with the intended client before production use.
- No real WorkBuddy account or upstream was used in the offline tests. Endpoint behavior, model availability, and terms of service may change.
