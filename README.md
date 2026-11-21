# Caddy SQLite leases

Registers a Caddy app (`sqlite_leases`) and HTTP middleware (`sqlite_lease`).

## Build

```sh
xcaddy build --with github.com/avalitan/caddy-sqlite-leases
```

This is a **local source module**, not a published GitHub repository. The module path is a placeholder for the intended repository. Requires Go dependencies to be downloaded on a machine with internet access.

## Global Caddy JSON app

```json
{
  "apps": {
    "sqlite_leases": {
      "database": "/var/lib/caddy/leases/leases.db",
      "lease_ttl": "24h",
      "cleanup_interval": "10m",
      "policy_uri_prefix": "urn:avalitan:caddy:lease:",
      "lease_policies": { "infrastructure": "72h", "persistent": "168h" },
      "host_overrides": { "proxmox01": "168h" },
      "admin_socket": "/var/lib/caddy/.local/share/caddy/admin.sock"
    }
  }
}
```

`cleanup_interval: "0s"` disables cleanup. If omitted, the default is 10 minutes. The default TTL is 24 hours.

## Registration success handler

`sqlite_lease` is a **pre-response action**, not a wrapper around the entire
registration request. It renews the SQLite lease **before** handing control to
the next handler. It requires a verified TLS client certificate, uses its CN and
URI SANs, and derives the outer route ID as `site_auth_<CN>` or `l4_auth_<CN>`.

**Do not place it at the beginning of `/init` or `/init4`.** It would renew a
lease even if the Admin API subsequently rejected registration.

Instead, add the handler at the start of **each successful Admin API
`reverse_proxy.handle_response` branch**, immediately before the existing
`static_response` handler:

```json
"handle": [
  { "handler": "sqlite_lease", "kind": "l7" },
  {
    "handler": "static_response",
    "status_code": 200,
    "headers": { "Content-Type": ["application/json"] },
    "body": "{\"status\":\"initialized\"}"
  }
]
```

Do this for **both** `initialized` (new route: Admin API 200) and
`already_initialized` (confirmed existing outer and nested route: Admin API
200) success responses. Use `"kind": "l4"` in the two equivalent `/init4`
branches. Leave all failure responses and `/status` unchanged.

If SQLite renewal fails, the handler returns HTTP 503 without executing the
success response. Unlike the previous buffered-response implementation, this
handler does not intercept or buffer reverse proxy responses. Registration and
lease renewal are not transactional: a failed DB write after a successful route
creation can leave an untracked route; retrying `/init` renews the lease via
the `already_initialized` success branch.

## SQLite

The database and parent directory are created on first startup. WAL mode is enabled. Inspect with:

```sh
sqlite3 -readonly /var/lib/caddy/leases/leases.db \
  'SELECT cn,kind,policy,datetime(last_seen,"unixepoch"),datetime(expires_at,"unixepoch") FROM leases;'
```

## Cleanup

A single worker per app instance periodically deletes expired outer routes via the local Caddy Admin API Unix socket. HTTP 200, 204, and 404 count as successful deletion. Lease renewals and deletions are serialized by a mutex to prevent cleanup/heartbeat races inside one module instance.

## Current limitations / next steps

- This is a **first implementation scaffold**, not yet compiled or tested against Caddy: Go dependencies were unavailable in the authoring environment.
- It assumes the existing registration flow returns 200 only after successful registration or verification; the response may already have been written when SQLite fails, so an error cannot reliably change the client's response.
- It does not change your existing Admin API authorization matchers.
- Database schema migration and reconciliation of orphaned routes are not implemented yet.
- Cleanup errors are retried at the next interval but currently not logged.
- The module does not yet protect against a manual Caddy reload replacing the dynamically created route tree.
- Run one active cleanup owner for the same Caddy Admin API and SQLite database.
