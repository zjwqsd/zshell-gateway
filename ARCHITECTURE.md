# Gateway architecture

```text
ChatGPT / MCP client
        |
        | HTTPS + OAuth
        v
+----------------------------------+
| zshell-gateway (Go)              |
|                                  |
| MCP :8765                        |
| OAuth                            |
| Tool registry                    |
| Live device registry             |
| name -> ShellCore session        |
+----------------+-----------------+
                 |
                 | private TCP :8767
                 | length-prefixed JSON
       +---------+----------+---------+
       |                    |         |
       v                    v         v
  Core "laptop"       Core "4090"  Core "project-b"
  workspace A         workspace B   workspace C
```

## Connection ownership

ShellCore always initiates the device connection. The gateway never scans for or dials ShellCore.

The registry contains zero or more active sessions keyed by the name declared by ShellCore. Names are unique while online; a duplicate handshake is rejected.

Calls to different devices may proceed independently. Calls and heartbeat traffic on one device session remain serialized by that session's connection lock.

## MCP routing

`device_list` is always available and reads the live registry.

All ShellCore-backed tools accept a `device` selector. With one online device the selector may be omitted. With multiple devices omission returns `DeviceRequired` rather than guessing a destination.

The MCP tool schema is stable: `device` is a plain string. Device membership is kept only in the live registry and resolved at call time, so connect/disconnect/rename does not require a tool-schema refresh.

## Device protocol v1

Each frame is:

```text
4-byte big-endian payload length
JSON payload
```

Maximum payload size: 16 MiB.

Handshake:

```text
ShellCore -> hello(protocol=1, token, device{name, workspace, os, arch, version})
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

The gateway currently probes each connection every three seconds. A failed transport is removed from the live registry immediately.
