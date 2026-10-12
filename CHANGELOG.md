# Changelog

## Unreleased

### Added

- Durable feedback intake with strict private HTTP parsing, normalization, consent, and submission idempotency.
- Atomic request and two-job acceptance in one versioned JSONL journal with exclusive locking, reserved capacity, crash recovery, physical retention, and administrative commands.
- Independent SMTP and Telegram workers with bounded retry cycles and persistent delivery outcomes.
- Verified SMTP TLS and safe MIME; Telegram notification through an authenticated fixed CONNECT proxy without personal data.
- Private Xray VPN container with a pinned core, bounded VLESS import, validated updates, and expiring last-known-good configuration.
- Non-root application and VPN images, private Compose topology, process health/readiness, and complete quality and synthetic transport checks.
- Go 1.26.9 quality foundation with pinned lint/architecture tools and enforcement scenarios.

### Fixed

- SMTP delivery supports servers that advertise `LOGIN` without `PLAIN`. Authentication selects advertised capabilities after verified TLS and does not retry rejected credentials through another mechanism.

- Container smoke inputs now belong to the host runner; closed container-only files remain owned by UID 65532. This permits native Linux CI to read its env and request files.

### Compatibility

- Adds private POST /feedback, health/readiness routes, configuration, journal version 1, and administrative CLI.
- No release, factory selection, live provider send, or deployment has been performed.
- Gateway integration and operator-registration email challenges remain separate tasks.
