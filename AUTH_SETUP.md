# Authentication setup

Public authentication belongs entirely to `zshell-gateway`.

Required secrets:

```text
ZSHELL_OAUTH_ADMIN_PIN      24-512 characters
ZSHELL_OAUTH_JWT_SECRET     exactly 64 hexadecimal characters
ZSHELL_DEVICE_TOKEN         24-512 characters
```

`ZSHELL_OAUTH_ADMIN_PIN` and `ZSHELL_OAUTH_JWT_SECRET` are only for MCP/OAuth clients and stay inside the Go gateway.

`ZSHELL_DEVICE_TOKEN` authenticates the private ShellCore connection. Configure the same value on exactly the ShellCore intended to connect to this gateway.
