# Gateway architecture

```text
                         zshell-gateway :8765
                                |
        +-----------------------+-----------------------+
        |                       |                       |
      /mcp                  /oauth/*               /device/ws
        |                                               |
   ChatGPT / MCP                               WebSocket devices
                                                /             \
                                 wss:// public /               \ ws:// LAN
                                              /                 \
                                         Core "laptop"     Core "server"
```

## One-port design

The gateway runs one `net/http` server. HTTP paths select the protocol handler, so MCP, OAuth and ShellCore WebSockets do not need separate ports.

The old dedicated device TCP transport has been removed. There is no length-prefixed raw-TCP listener.

## Device connection

1. ShellCore opens `ws://.../device/ws` or `wss://.../device/ws`.
2. The HTTP Upgrade request carries `Authorization: Bearer <device token>`.
3. After the WebSocket upgrade, Core sends `hello(protocol=3, device={...})`.
4. The gateway validates metadata and rejects duplicate live device names.
5. The persistent WebSocket carries JSON/text control messages and raw binary file-transfer frames.

Application messages:

```text
Gateway -> call(id, operation, arguments)
Core    -> result(id, payload)

Gateway -> ping(id)
Core    -> pong(id)

Gateway -> transfer_* control messages
Core    -> transfer_* state messages
Core    -> ZTF1 binary chunks -> Gateway -> target Core
```

WebSocket protocol control frames are handled independently by the WebSocket implementation.

## Transfer routing

The device manager owns transfer lifecycle and routes binary frames between two already-connected sessions. A transfer reserves both device names until it reaches `completed`, `failed`, or `cancelled`. Gateway stores only metadata/progress; file payload is synchronously forwarded to the target session so slow target I/O naturally backpressures the source instead of growing an unbounded Gateway buffer.

The transfer state sequence is:

```text
preparing -> transferring -> verifying -> completed
                    \-> failed / cancelled
```

The source computes SHA-256 while reading. The target independently hashes bytes written to `.zshell-part`. Gateway sends a commit only after every declared source byte has been relayed, and marks completion only after target size/hash matches the source.

## MCP routing

`device_list` reads the live registry. Every Core-backed tool accepts an optional `device` selector:

- zero devices: `NoDevice`
- one device: omission resolves to that device
- multiple devices: omission returns `DeviceRequired`

The MCP tool schema keeps `device` as a plain string so device connect/disconnect/rename does not require schema refresh.

## Security boundary

Public deployments should terminate TLS at the public endpoint and use `wss://` from Core. A typical Cloudflare Tunnel or reverse-proxy deployment forwards the public hostname to `127.0.0.1:8765`.

The shared device secret is sent only in the authenticated WebSocket upgrade. On `wss://`, TLS protects it in transit. Plain `ws://` is reserved for trusted LAN use.

Incoming WebSocket payloads are limited to 8 MiB and the HTTP server applies bounded header and read-header handling.
