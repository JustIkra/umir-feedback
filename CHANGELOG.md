# Changelog

## Unreleased

- Correct the README: with `FEEDBACK_AUTH_URL` the unscoped `/feedback` route answers 503 `FeedbackStorageUnavailable` instead of serving legacy intake. Record the single-VM (synchronized clocks) requirement and the zero `iat` tolerance, and pin it with a test: `iat == now+1s` is rejected, `iat == now` is accepted, `exp` must equal `iat`+60.

- Log delivery outcomes without personal data: the worker reports each transition (`sent`, `retry`, `failed`, `unknown`) to a `DeliveryObserver`, written as a `feedback.delivery` slog record with channel, result code, attempt, job and submission IDs only; the scoped handler now logs 503 `FeedbackStorageUnavailable` like the inherited one. Transitions made by the journal itself (retry budget exhausted on claim or startup recovery) are not logged yet. The worker stop policy is unchanged.

- Set the SMTP `Reply-To` header only for an address made entirely of ASCII; for a non-ASCII address such as `иван@пример.рф` the header is omitted and the address stays in the UTF-8 body, so the message no longer gets a permanent 5xx from relays without SMTPUTF8. Intake validation is unchanged.

- Retry the VPN subscription download with backoff after a failed start or scheduled refresh (30 s to 15 min without a fresh cache, 5 to 15 min with one, jitter of 20 percent) and log `vpn.refresh.failed` / `vpn.refresh.ok` as JSON without the subscription URL; a success returns to the ordinary refresh interval and no retry opens a direct route.

- Fetch the Auth JWKS once per refresh without holding the client lock: callers share one network call with its own deadline, known keys are served from the snapshot while a refresh runs, the snapshot refreshes in the background after 30 s and is rejected after `FEEDBACK_AUTH_KEYS_MAX_STALE` (default 5 minutes, range 30 seconds to 10 minutes), and an unknown key id causes at most one refetch per second.

- Align CI with the completed Auth mail cutover: remove references to the removed OTP probe image and script. Retain Go quality/race checks, pinned VPN validation and real synthetic Feedback runtime smoke.

- Add a bounded read-only journal projection for real legacy anonymous records
  and archived challenge/job deadlines. Copy facts under the existing writer
  lock and expose provider IDs only as owner-keyed HMAC proofs. Reject scoped
  and fresh anonymous records; preserve journal, worker and retention behavior.

- Repair A8 legacy snapshot job migration and global snapshot job ownership validation. Preserve delivery identity through compaction, continued transitions and repeated recovery.
- Reject fresh fixture observations across owner clock movement. Preserve exact cached replay bytes and enforce the approved 100 ms polling minimum within the original wait budget.
- Validate the full scoped export response and frozen page shape before output. Reject trailing values, duplicate fields, malformed receipts and foreign restaurant rows without emitting bytes.

- Add frozen C2 Feedback v2 tenant intake and authorized scope reads/export with strict Auth Ed25519 verification and separate discovery credentials. Persist namespace fingerprints, command IDs and independent channel job UUIDs in journal version 2; retain old records explicitly anonymous with stable owner migration identities.
- BREAKING CHANGE: operator export requires an explicit Gateway Admin session and restaurant. Older binaries cannot read journal version 2. Remove automatic runtime purge; manual legacy maintenance preserves scoped facts.
- Add test-only RPC1.1.0 live pair observation, controlled owner clock, one-shot real commit/reply fault barriers and synthetic real SMTP/CONNECT notification correlation. Auth bytes remain unchanged. Separate feedback-auth provisioning, closed owner profiles and real J1/E acceptance remain pending.
- Extend capacity reservation to include version 2 namespace/job fields. The maximal-state and complete ten-cycle regression now include the new wire metadata; its bounded fixture uses 96 KiB.


- Transferred active OTP, crypto, approved registration MIME and auth SMTP ownership to Auth in the isolated A4 candidate. Removed competing private challenge routes, worker, config, Compose overlay and old probe.
- Retained strict read-only decoding and unchanged compaction of historical challenge journal snapshots. Feedback intake and independent SMTP/Telegram jobs keep their existing ownership.

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
