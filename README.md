# zshell-gateway

Go protocol gateway for zshell.

The gateway exposes one HTTP service. MCP, OAuth, health checks and ShellCore device connections all share the same listen address and are separated by HTTP paths.

## Endpoints

```text
/mcp          MCP Streamable HTTP
/oauth/*      OAuth authorization endpoints
/.well-known/* OAuth/MCP metadata
/device/ws    ShellCore WebSocket transport
/healthz      process health
```

Default listen address:

```text
127.0.0.1:8765
```

There is no separate ShellCore TCP listener.

## ShellCore transport

ShellCore connects outbound to `/device/ws` and authenticates the WebSocket upgrade with:

```text
Authorization: Bearer <ZSHELL_DEVICE_TOKEN>
```

After the upgrade, the same WebSocket carries the protocol-v3 device hello, calls, results, liveness messages and cross-device transfer traffic. Device names are supplied by ShellCore and must be unique while connected.

Recommended URLs:

```text
Public: wss://zshell.example.com/device/ws
LAN:    ws://192.168.1.20:8765/device/ws
```

Use `wss://` for any untrusted network. `ws://` is intended only for trusted LAN use because its traffic and device token are not encrypted.

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
```

No additional public device port is required.

## Cross-device file transfer

Gateway exposes three transfer tools:

```text
file_transfer
file_transfer_status
file_transfer_cancel
```

`file_transfer` names both source and target devices explicitly. Gateway asks both Cores to prepare their local paths, then streams 256 KiB file chunks as raw WebSocket binary frames:

```text
source Core -> Gateway -> target Core
```

Gateway does not load the whole file into memory, persist it to disk, base64-encode it, or place file bytes in MCP responses. The synchronous target WebSocket write provides backpressure to the source. Control messages remain JSON/text frames.

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
- Public ShellCore connections must use `wss://` so TLS protects the token and device traffic.
- WebSocket messages are capped at 8 MiB.
- The HTTP server applies header/read timeouts; the device registry rejects duplicate live names.
- OAuth dynamic client registrations are persisted across Gateway restarts. Only client metadata is stored; access tokens, Admin PINs and authorization codes are not written to this file.
- OAuth redirect URIs must use HTTPS, except loopback HTTP callbacks. Authorization still requires an exact match to the registered redirect URI.
- Stopping a Core closes its WebSocket and removes the device from the live registry.
