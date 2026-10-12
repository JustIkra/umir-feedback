# Changelog

## Unreleased

- Merged the registration challenge candidate through PR #3 and the shared SMTP compatibility fix through PR #4. OTP remains disabled for auth #18 migration; form and registration delivery now share the advertised PLAIN/LOGIN transport.

- Recorded auth extraction #18 as the target owner of account/session, OTP and auth mail. Registration code in this branch is a disabled transfer candidate; coordinated reuse, single-owner migration and actual SMTP mechanism/client checks precede enablement.

- Added optional private registration OTP begin/resend/verify/complete endpoints with shared bearer authentication, stable operation IDs, bounded limits, strict ten-minute expiry and 24-hour proof recovery.
- Persisted challenge/outbox snapshots in the existing JSONL with HMAC digests, AES-GCM payloads, reserved three-attempt OTP capacity, replay retention and physical secret scrubbing. Cleanup continues when OTP is disabled. Old binaries cannot read a journal after new challenge records are written.
- Added registration HTML/plain-text MIME with the exact Figma PNG logo, original Moscow expiry and required runtime operator details. Reused verified SMTP transport with a separate challenge recipient and worker. Added optional closed-file Compose mounts; the feature is disabled by default.

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
