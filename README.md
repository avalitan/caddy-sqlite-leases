# Caddy SQLite leases — initial module scaffold

Registers a Caddy app (`sqlite_leases`) and HTTP middleware (`sqlite_lease`).

## Build

```sh
xcaddy build --with github.com/avalitan/caddy-sqlite-leases=/path/to/caddy-sqlite-leases
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

## Registration middleware

Place this handler **before** the existing `/init` registration logic in the Caddy route's handler chain:

```json
{ "handler": "sqlite_lease", "kind": "l7" }
```

Use `"kind": "l4"` on `/init4`. The handler wraps the downstream registration flow, checks for an authenticated TLS client certificate, and renews the SQLite lease **only if the downstream handler returns HTTP 200**. It uses the certificate's CN and URI SANs, not untrusted client input. It derives the allowed outer route ID as `site_auth_<CN>` or `l4_auth_<CN>`.

**Important integration requirement:** Both a new registration and the existing `already_initialized` path must finish with HTTP 200; the middleware doesn't independently verify the Caddy route exists. Place it only on the registration routes, not `/status` or generic Admin API proxy paths.

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
