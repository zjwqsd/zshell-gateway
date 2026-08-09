# zshell-gateway

Go-based public protocol layer for zshell.

The gateway owns MCP, OAuth and the ShellCore device registry. It contains no local shell execution implementation.

## Responsibilities

- MCP Streamable HTTP endpoint on `ZSHELL_MCP_LISTEN` (default `127.0.0.1:8765`)
- OAuth Authorization Code + PKCE + dynamic client registration
- private ShellCore TCP listener on `ZSHELL_DEVICE_LISTEN` (default `0.0.0.0:8767`)
- authentication of ShellCore with `ZSHELL_DEVICE_TOKEN`
- concurrent registration of multiple ShellCore devices
- routing MCP calls by the device-provided name
- heartbeat detection and immediate disconnect cleanup

## Device identity

Each ShellCore must provide a unique `ZSHELL_DEVICE_NAME` during its handshake.

The gateway never generates or changes device names. If a name is already online, another ShellCore using the same name is rejected.

`device_list` returns the current live registry, including each device's:

- `name`
- `workspace`
- `os`
- `arch`
- `version`

Every normal MCP tool has an optional `device` selector. When exactly one device is online it may be omitted. When multiple devices are online it is required.

The `device` field is always a plain string in the MCP schema. Online names are intentionally not encoded as a schema `enum`: clients may cache tool schemas while devices connect, disconnect, or rename. The gateway validates the requested name against its live registry at call time.

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

The same `ZSHELL_DEVICE_TOKEN` must be configured on every ShellCore allowed to join this gateway.

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

ShellCore instances may start before or after the gateway and reconnect automatically.

## Network

Expose only the MCP HTTP service publicly:

```text
mcp.example.com -> http://127.0.0.1:8765
```

Port `8767` is the ShellCore transport. The current device protocol uses a shared token over raw TCP, so this port should be reachable only through a trusted network, VPN, private tunnel or equivalent protected path.

## Health behavior

`GET /healthz` reports gateway process health, not device availability. It remains `ok` with zero connected devices.
