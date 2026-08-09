# zshell-gateway

Go-based public protocol layer for zshell.

The gateway owns MCP, OAuth and the single ShellCore connection. It contains no local shell execution implementation.

## Responsibilities

- MCP Streamable HTTP endpoint on `ZSHELL_MCP_LISTEN` (default `127.0.0.1:8765`)
- OAuth Authorization Code + PKCE + dynamic client registration
- one private ShellCore TCP listener on `ZSHELL_DEVICE_LISTEN` (default `0.0.0.0:8767`)
- authentication of ShellCore with `ZSHELL_DEVICE_TOKEN`
- forwarding MCP tool calls to the connected ShellCore
- heartbeat detection and disconnect cleanup

Exactly one ShellCore can be connected at a time. A second ShellCore is rejected until the current one disconnects.

The gateway starts and stays healthy even when no ShellCore is online. In that state every MCP tool call returns:

```text
无设备连接
```

## Required environment

```text
ZSHELL_PUBLIC_BASE_URL=https://mcp.example.com
ZSHELL_OAUTH_ADMIN_PIN=<24-512 character secret>
ZSHELL_OAUTH_JWT_SECRET=<exactly 64 hexadecimal characters>
ZSHELL_DEVICE_TOKEN=<24-512 character shared device secret>
```

Optional:

```text
ZSHELL_MCP_LISTEN=127.0.0.1:8765
ZSHELL_DEVICE_LISTEN=0.0.0.0:8767
```

The same `ZSHELL_DEVICE_TOKEN` must be configured on ShellCore.

## Build

```bash
go build ./cmd/zshell-gateway
```

## Run

Linux / Android Debian:

```bash
export ZSHELL_PUBLIC_BASE_URL=https://mcp.example.com
export ZSHELL_OAUTH_ADMIN_PIN='replace-with-a-long-secret'
export ZSHELL_OAUTH_JWT_SECRET='replace-with-64-hex-characters'
export ZSHELL_DEVICE_TOKEN='replace-with-a-long-device-secret'
./zshell-gateway
```

PowerShell:

```powershell
$env:ZSHELL_PUBLIC_BASE_URL = "https://mcp.example.com"
$env:ZSHELL_OAUTH_ADMIN_PIN = "replace-with-a-long-secret"
$env:ZSHELL_OAUTH_JWT_SECRET = "replace-with-64-hex-characters"
$env:ZSHELL_DEVICE_TOKEN = "replace-with-a-long-device-secret"
.\zshell-gateway.exe
```

ShellCore may start before or after the gateway. It reconnects automatically.

## Cloudflare Tunnel

Expose only the MCP HTTP service:

```text
mcp.example.com -> http://127.0.0.1:8765
```

Port `8767` is the private ShellCore transport and should remain on a trusted LAN/private VPN. The current device transport uses a shared token over raw TCP and does not provide TLS itself.

## Health behavior

`GET /healthz` reports the gateway process health, not device availability. Therefore it continues to return `ok` while no ShellCore is connected.
