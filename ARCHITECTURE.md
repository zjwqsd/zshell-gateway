# Gateway architecture

```text
ChatGPT / MCP client
        |
        | HTTPS + OAuth
        v
+---------------------------+
| zshell-gateway (Go)       |
|                           |
| MCP :8765                 |
| OAuth                     |
| Tool registry             |
| Single-device manager     |
+-------------+-------------+
              |
              | private TCP :8767
              | length-prefixed JSON
              v
        zshell-core (Zig)
```

## Connection ownership

ShellCore always initiates the device connection. The gateway never scans for or dials ShellCore.

The device manager has zero or one active session. Tool requests are serialized over that session. Heartbeats are also serialized so a legitimate long-running tool invocation cannot be mistaken for a dead connection.

## Device protocol v1

Each frame is:

```text
4-byte big-endian payload length
JSON payload
```

Maximum payload size: 16 MiB.

Handshake:

```text
ShellCore -> hello(protocol=1, token, device info)
Gateway   -> hello_ack(accepted=true/false)
```

Calls:

```text
Gateway   -> call(id, operation, arguments)
ShellCore -> result(id, payload)
```

Liveness:

```text
Gateway   -> ping(id)
ShellCore -> pong(id)
```

A transport failure removes the current session immediately. Subsequent MCP tool calls return `无设备连接` until a ShellCore reconnects.
