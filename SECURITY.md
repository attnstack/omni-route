# Security Policy

## Reporting

Please report vulnerabilities via GitHub Security Advisories
("Report a vulnerability" on the Security tab) rather than public issues.

## Scope and design notes

- The gateway holds upstream provider keys in process memory only, read from
  environment variables at startup. They are never logged, never echoed in
  error bodies, and never persisted.
- Client authentication (`api_keys`) is optional but strongly recommended in
  any deployment that is not loopback-only. Comparison is constant-time.
- The response cache keys on the full request body (minus stream flag, plus
  explicit model when present). If different callers can see each other's
  prompts, either disable the cache (`cache.enabled: false`) or require
  per-caller API keys so keys can be mixed into the cache key via distinct
  explicit models. When in doubt, keep the cache off for multi-tenant
  deployments.
- `POST /admin/reload` is protected by `admin_key_env`; do not expose the
  admin port to untrusted networks.

## Supported versions

Only the latest tagged release receives security fixes.
