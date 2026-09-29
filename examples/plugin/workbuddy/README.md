# WorkBuddy provider plugin (experimental)

This standalone shared-library plugin adds a `workbuddy` provider **without changing the CLIProxyAPI core**. It supports the international (`intl`) and China (`cn`) portals, browser login and token refresh, per-account model lists, OpenAI Chat Completions (streaming and non-streaming), and credit balance queries. The host's existing translators can serve other downstream protocols where supported.

## Build and start

Build with Go and a working C toolchain (`CGO_ENABLED=1`) for the **same OS and architecture as the server**. From the repository root:

```bash
# Linux example; use workbuddy.dylib on macOS.
go build -buildmode=c-shared -o plugins/workbuddy.so ./examples/plugin/workbuddy/go
```

On Windows, use a Windows-capable C toolchain and build a DLL instead:

```powershell
go build -buildmode=c-shared -o plugins/workbuddy.dll ./examples/plugin/workbuddy/go
go build -o plugins/workbuddy-server.exe ./cmd/server
```

Enable the plugin in your **local** `config.yaml` (do not also configure `workbuddy` as an `openai-compatibility` provider):

```yaml
plugins:
  enabled: true
  path:
    - /absolute/path/to/plugins/workbuddy.so # Use an absolute .dll path on Windows.
  configs:
    workbuddy:
      enabled: true
      priority: 1
```

Start the server from the repository root so it finds the intended config and auth store. On Windows you can run it **without a visible terminal window**; redirected logs remain local under the ignored `plugins/` directory:

```powershell
Start-Process -FilePath (Resolve-Path 'plugins/workbuddy-server.exe').Path `
  -ArgumentList '--config', (Resolve-Path 'config.yaml').Path `
  -WorkingDirectory (Get-Location).Path -WindowStyle Hidden `
  -RedirectStandardOutput 'plugins/workbuddy-server.stdout.log' `
  -RedirectStandardError 'plugins/workbuddy-server.stderr.log' -PassThru
```

`-WindowStyle Hidden` only hides the console: **the server process must remain running** for chat and credit refreshes. It does not auto-start after reboot or sign-out; set up a Windows Scheduled Task or a service separately if needed. Before replacing an in-use DLL, stop the server and restart it after rebuilding. Do not commit `config.yaml`, `plugins/*`, local logs, credentials, or management keys.

## Sign in and use models

In the management UI, start the WorkBuddy OAuth flow for the correct region. The equivalent management endpoint is `GET /v0/management/oauth/auth-url?provider=workbuddy&realm=cn` (use `realm=intl` for international accounts). Open the returned URL and poll the management auth-status endpoint with its returned state; the UI normally does the polling. Omitting `realm` selects `intl`, **not** `cn`.

Each region/account has its own credential in the local `auths/` store. Recent WorkBuddy desktop builds encrypt their tokens, so desktop credential import is **not** supported. Older unencrypted auth files may be placed in `auths/` with `provider`, `uid`, `realm`, `accessToken`, `refreshToken`, and optional `product` (`workbuddy`, `cli`, or `vscode`). Protect these files; they contain bearer and refresh tokens. The plugin uses the live `/v3/config` model list for a credential where available and otherwise falls back to a bundled snapshot; restart after upstream model changes if a new model is not routed.

For Claude Code, Pi, or another client, point the client's compatible API endpoint at your local CLIProxyAPI server and use the **proxy client API key** for chat (not the WorkBuddy access token or management key). Configure only the intended profile; there is no need to change an existing default provider just to test WorkBuddy.

## Check remaining credits

The quota provider queries WorkBuddy's billing meter for the selected WorkBuddy credential. Results show remaining, used and total **credits** (not currency or token counts), plus the remaining fraction for each package. Billing results are not cached or persisted by this plugin. Its example query reads up to 100 active packages; accounts exceeding that limit require pagination support.

The authenticated Management API is `POST /v8/management/credentials/quota/fetch` with JSON `{"auth_index":"<index>"}`. Find that credential's `auth_index` via `GET /v0/management/auth-files`. This endpoint requires the **management key**, not the proxy client API key. Keep management bound to localhost; granting this key to another app gives it broad management access, not just credit read access.

### CC Switch Usage Query (optional)

On the WorkBuddy provider card (Claude Code or Pi), enable **Usage Query**, choose **Custom**, and set the query's **Base URL** override to your local management URL (for example `http://127.0.0.1:8317`). Set its **API Key** override to your local management key; do **not** change the provider's proxy client API key. Replace `REPLACE_WITH_AUTH_INDEX` below with your WorkBuddy credential's actual `auth_index`, then paste this script into the custom script editor:

```javascript
({
  request: {
    url: "{{baseUrl}}/v8/management/credentials/quota/fetch",
    method: "POST",
    headers: {
      "Authorization": "Bearer {{apiKey}}",
      "Content-Type": "application/json"
    },
    body: JSON.stringify({ auth_index: "REPLACE_WITH_AUTH_INDEX" })
  },
  extractor: function (response) {
    if (!response || !Array.isArray(response.summary)) {
      return { isValid: false, invalidMessage: "WorkBuddy quota response unavailable" };
    }
    var metrics = {};
    for (var i = 0; i < response.summary.length; i++) {
      var item = response.summary[i];
      metrics[item.key] = item.value;
    }
    if (typeof metrics.remaining !== "number") {
      return { isValid: false, invalidMessage: "Remaining credits missing" };
    }
    return {
      isValid: true,
      planName: "WorkBuddy",
      remaining: metrics.remaining,
      used: metrics.used,
      total: metrics.total,
      unit: "credits",
      extra: (response.groups || []).length + " packages"
    };
  }
})
```

Use **Test Script** before saving. CC Switch only auto-refreshes an **active** provider; manually refresh an inactive card. The management key is saved in CC Switch's **local profile data** as the Usage Query override. Do not sync, share, screenshot, or commit that data. If storing a privileged management key in CC Switch is not acceptable, query the Management API directly instead; the plugin does not expose a separately authorized, read-only billing endpoint.

## Troubleshooting

- **Connection refused / no credit refresh:** Check that CLIProxyAPI is still listening on the configured local port. CC Switch running by itself does not run the proxy; a hidden server is still a process that can be stopped. Restart the server, then use **Test Script** or manually refresh the inactive card.
- **401 / 403 from Management API:** Check the Usage Query API Key **override** (management key), the management listener's local-access settings, and the selected credential. The proxy client API key cannot call management quota endpoints.
- **No quota provider / stale models after an update:** Rebuild the plugin for the current host platform and restart the host after replacing the DLL/library. Check the server log for plugin registration errors.
- **Login or billing fails for CN:** Select `realm=cn`, use a CN credential, and check the host's network/proxy access. CN login/chat and billing use different upstream hosts; do not send an international token to the CN billing endpoint.
- **Unexpected zero, empty, or failed balance:** Verify the selected `auth_index` still belongs to the intended WorkBuddy account. The parser rejects malformed or rejected billing responses rather than treating them as zero credits; a legitimately empty package list is different.

## Scope and limitations

- Outbound traffic uses the host's `host.http` callbacks so the host transport, global proxy, and request logging policy apply. The callback API does **not** supply the selected auth's proxy URL; per-account proxy routing is therefore not supported. Do not rely on it for region-specific exit IPs. Keep international and China credentials separate and set the server's global proxy accordingly.
- Chat input supports ordinary text and tools, with minimal compatibility fixes for developer roles, `max_completion_tokens`, object `tool_choice`, and DeepSeek reasoning. It does not implement all of workbuddy2api-hub's history repair, WAF sanitization, web-agent features, billing/check-in, or server-side web tools. Tool-call streaming and cross-protocol translation should be validated with the intended client before production use. Token counting is a placeholder, **not** a real tokenizer.
- Automated tests use simulated upstream responses, not a real WorkBuddy account. Endpoint behavior, model availability, and terms of service may change.
