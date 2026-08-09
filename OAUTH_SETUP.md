# OAuth setup

The Go gateway exposes:

```text
/.well-known/oauth-protected-resource
/.well-known/oauth-protected-resource/mcp
/.well-known/oauth-authorization-server
/.well-known/oauth-authorization-server/mcp
/oauth/register
/oauth/authorize
/oauth/token
/mcp
```

Supported flow:

```text
OAuth Authorization Code
+ Dynamic Client Registration
+ PKCE S256
+ local admin-PIN approval
+ Bearer JWT access token
```

Cloudflare Tunnel should point only to `http://127.0.0.1:8765`. ShellCore does not implement or receive OAuth traffic.
