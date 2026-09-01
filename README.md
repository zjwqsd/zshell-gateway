# zshell-gateway

Go protocol gateway for zshell.

The gateway exposes one HTTP service. MCP, OAuth, health checks and ShellCore device connections all share the same listen address and are separated by HTTP paths.

## Endpoints

```text
/mcp          MCP Streamable HTTP
/oauth/*      OAuth authorization endpoints
/.well-known/* OAuth/MCP metadata
/device/ws    ShellCore WebSocket transport
/device/http  ShellCore HTTP transport
/healthz      process health
```

Default listen address:

```text
127.0.0.1:8765
```

There is no separate ShellCore TCP listener.

## ShellCore transport

Gateway accepts both `/device/ws` and `/device/http`. Both authenticate with:

```text
Authorization: Bearer <ZSHELL_DEVICE_TOKEN>
```

The Core selects transport from `ZSHELL_GATEWAY_URL`: `ws://`/`wss://` use WebSocket and `http://`/`https://` use HTTP. There is no automatic fallback. Device names are supplied by ShellCore and must be unique while connected.

WebSocket keeps the existing protocol-v3 wire format. HTTP creates a device session, uses long polling for Gateway-to-Core control messages, POST requests for Core-to-Gateway control messages, and separate raw binary endpoints for file chunks.

Recommended public URLs:

```text
WebSocket: wss://zshell.example.com/device/ws
HTTP:      https://zshell.example.com/device/http
```

Use TLS (`wss://` or `https://`) on untrusted networks.

## Required environment

```text
ZSHELL_PUBLIC_BASE_URL=https://zshell.example.com
ZSHELL_OAUTH_ADMIN_PIN=<24-512 character secret>
ZSHELL_OAUTH_JWT_SECRET=<exactly 64 hexadecimal characters>
ZSHELL_DEVICE_TOKEN=<24-512 character device secret>
```

Optional:

```text
ZSHELL_MCP_LISTEN=127.0.0.1:8765
ZSHELL_OAUTH_CLIENTS_FILE=/var/lib/zshell-gateway/oauth-clients.json
```

For a direct LAN deployment, bind the single HTTP service to a LAN interface, for example:

```text
ZSHELL_MCP_LISTEN=0.0.0.0:8765
```

## Public deployment

A reverse proxy or Cloudflare Tunnel only needs to expose the single HTTP service:

```text
zshell.example.com -> http://127.0.0.1:8765
```

Then both clients use the same hostname:

```text
ChatGPT -> https://zshell.example.com/mcp
Core    -> wss://zshell.example.com/device/ws
       or https://zshell.example.com/device/http
```

No additional public device port is required.

## Device capabilities

Protocol v3 device hello messages may include `device.capabilities`. Gateway owns all MCP tool schemas; devices only declare capability IDs. This keeps the tool/security boundary in Gateway while allowing Android, Linux and Windows nodes to expose different native features.

Current capability groups include:

```text
core.exec
core.jobs
core.shell
files.read
files.write
files.transfer
browser
device.info
apps.read
apps.launch
apps.stop
apps.current
screen.capture
ui.inspect
ui.input
android.intent
```

`device_list` returns the effective capability list for each live node. Gateway checks the required capability before forwarding an operation and returns `CapabilityUnsupported` locally when the selected node cannot perform it. Older protocol-v3 desktop Cores that omit `capabilities` retain the legacy exec/jobs/shell/files/browser set.

Native-device tools currently registered by Gateway are:

```text
device_info
app_list
app_info
app_launch
app_stop
app_current
screen_capture
ui_dump
ui_tap
ui_swipe
ui_text
ui_keyevent
android_intent_start
```

The first group is platform-neutral where a Core implements it. `android_intent_start` remains Android-specific. UI tools are capability-gated rather than assumed on every desktop because Wayland/X11/Windows expose very different automation surfaces.

## Cross-device file transfer

Gateway exposes three transfer tools:

```text
file_transfer
file_transfer_status
file_transfer_cancel
```

`file_transfer` names both source and target devices explicitly. Gateway asks both Cores to prepare their local paths and relays chunks through the transport of each peer:

```text
source Core -> Gateway -> target Core
```

All combinations are supported: WebSocket -> WebSocket, WebSocket -> HTTP, HTTP -> WebSocket, and HTTP -> HTTP. WebSocket uses 1 MiB `ZTF1` binary frames. HTTP file data uses raw `application/octet-stream` requests (currently 1 MiB chunks) with explicit sequence/ack handling.

Gateway does not load the whole file into memory, persist it to disk, base64-encode it, or place file bytes in MCP responses. Transport writes/acknowledgements provide backpressure to the source. Control messages remain protocol-v3 JSON messages.

The target writes to `<destination>.zshell-part`. Completion requires matching byte counts and SHA-256 digests from source and target before the target file is committed. Cancellation/failure removes the temporary file. In protocol v3, one device can participate in at most one active transfer at a time.

`overwrite` defaults to false. If the destination already exists, target preparation fails before streaming begins.

## Browser-capability behavior

Browser tools remain registered at Gateway level so one Gateway can serve a mix of browser-enabled and browser-disabled ShellCore devices without changing the MCP tool schema as devices connect and disconnect.

For a Core started without `--browser`:

- `browser_status` reports `enabled=false`
- any other `browser_*` call returns `BrowserFeatureDisabled` with an explicit message to restart that Core with `--browser`

A browser-enabled Core validates `agent-browser` and Chrome/Chromium before it connects, so Gateway never sees a device claiming enabled browser support with missing startup dependencies.

## Build and test

```bash
go test ./...
go build ./cmd/zshell-gateway
```

## Security notes

- Keep `ZSHELL_DEVICE_TOKEN` high-entropy and out of logs/source control.
- Public ShellCore connections must use `wss://` or `https://` so TLS protects the token and device traffic.
- WebSocket messages are capped at 8 MiB; HTTP control bodies and transfer chunks are independently bounded.
- The HTTP server applies header/read timeouts; the device registry rejects duplicate live names.
- OAuth dynamic client registrations are persisted across Gateway restarts. Only client metadata is stored; access tokens, Admin PINs and authorization codes are not written to this file.
- OAuth redirect URIs must use HTTPS, except loopback HTTP callbacks. Authorization still requires an exact match to the registered redirect URI.
- Stopping a Core closes its active transport session and removes the device from the live registry.
