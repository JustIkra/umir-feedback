# Umir Feedback

Independent Go service for durable feedback intake and two notification channels. SMTP sends the full request to one configured operator address. Telegram sends only the submission ID, Moscow time, and a reminder to check that mailbox. Telegram uses a separate private Xray CONNECT container.

The service has no published release. Gateway routing, factory assembly, and registration email challenges remain separate integration tasks. No real provider notification was sent during implementation.

## Architecture

Dependencies point toward business rules:

- `internal/domain` owns feedback values, field validation, identifiers, and retry limits.
- `internal/application` owns intake, content fingerprints, consent, and delivery decisions. It defines the repository, sender, clock, normalization, and jitter ports that it consumes.
- `internal/adapters/journal` implements one durable JSONL journal and its exclusive lock. SMTP, Telegram, HTTP, runtime configuration, and VPN mechanisms are separate adapters.
- `bootstrap` wires implementations and owns the HTTP listener, two workers, daily retention, cancellation, and joins.
- `cmd/server`, `cmd/feedback`, and `cmd/telegram-vpn` are process entry points. Tests and synthetic transport data stay outside production mechanisms.

The service does not import other service modules. Gateway owns public Origin/CSRF checks, rate limits, accounts, and sessions. Feedback owns stored requests and notification jobs. SMTP and Telegram failures do not block durable intake. A journal failure stops delivery and makes intake unavailable.

## HTTP contract

The service listens at `127.0.0.1:8084` by default. Set `FEEDBACK_LISTEN_ADDRESS` for another address. Compose uses `0.0.0.0:8084` inside the private container and publishes no host port. Gateway will forward its exact public `POST /api/feedback` route to private `POST /feedback`.

```json
{
  "submissionID": "123e4567-e89b-42d3-a456-426614174000",
  "name": "Operator",
  "email": "user@example.org",
  "phone": "+7 (999) 123-45-67",
  "message": "Please contact me.",
  "consent": {"accepted": true, "documentVersion": "current-version"},
  "website": ""
}
```

`phone` and `website` can be omitted. The body is limited to 16 KiB and requires `application/json`. Unknown or duplicate fields, case-mismatched keys, null values, trailing JSON, invalid UTF-8, and unpaired surrogate escapes receive `400`. A submission ID must be UUID v4; the canonical stored form is lowercase.

| Result | Status and body |
| --- | --- |
| New request and both jobs durably committed | `201 {"submissionID":"…","receivedAt":"…+03:00"}` |
| Same retained ID and normalized content | `200` with the original receipt |
| Same ID with different content | `409 SubmissionIdentityConflict` |
| Invalid JSON, schema, or ID | `400 InvalidRequest` |
| Invalid fields | `422 InvalidFeedback` with `fieldErrors` |
| Missing consent or obsolete version | `422 ConsentRequired` with `ConsentRequired` or `ConsentOutdated` field code |
| Wrong method, media type, or oversized body | `405 MethodNotAllowed`, `415 UnsupportedMediaType`, or `413 PayloadTooLarge` |
| Storage commit not confirmed | `503 FeedbackStorageUnavailable` |

Errors use `{"code","message","fieldErrors"?,"requestID"}`. Responses use JSON and `Cache-Control: no-store`. Public `429` limits belong to gateway; this private service does not duplicate public session or CSRF rules.

Names are trimmed, normalized to NFC, and limited to 100 Unicode code points. Messages are trimmed at the edges and limited to 2000 code points; internal LF and tab are preserved. Other control characters are rejected. Email uses a single unquoted address, at most 254 bytes, with one `@` and a dotted domain; only the domain becomes lowercase. Quoted local parts, display names, address lists, comments, and domain literals are rejected. A supplied phone permits ASCII digits, spaces, `+`, `-`, `(`, and `)`, at most 32 code points and 10–15 digits.

New requests require the configured consent version. The journal records purpose `feedback-reply` and acceptance time. An exact retained replay succeeds after the configured version changes. A nonempty `website` is a honeypot: it returns a plausible `201` without storage or jobs. After deletion or retention, the removed ID has no deduplication guarantee.

`201` confirms storage, not provider delivery. If a response is lost, repeat the original ID and content. SMTP and Telegram can accept an attempt before a crash or timeout prevents local confirmation. A later attempt can therefore duplicate a notification. A durably stored `sent` state is not retried.

## Configuration and local execution

Use Go 1.26.9. Set the variables below in a private process environment, then run `go run ./cmd/server`. Create the journal directory first with mode `0700`, owned by the process UID/GID. The absolute journal path and its ancestors must not use symlinks. Existing unsafe files or directories cause startup failure; the service does not reset them.

| Variable | Requirement or default |
| --- | --- |
| `FEEDBACK_STORE_PATH` | Required absolute canonical journal file path |
| `FEEDBACK_MAX_STORE_BYTES` | Required; 1 MiB to 1 TiB; bounded journal, future state reserve, and compaction envelope |
| `FEEDBACK_RETENTION_DAYS` | Required; 1–3650 days |
| `FEEDBACK_CONSENT_VERSION` | Required nonempty version, at most 128 bytes |
| `FEEDBACK_LISTEN_ADDRESS` | `127.0.0.1:8084` |
| `FEEDBACK_RETRY_MAX_ATTEMPTS` | `10`; range 1–10 |
| `FEEDBACK_RETRY_MAX_AGE` | `24h`; at most 24 hours per automatic retry cycle |
| `FEEDBACK_RETRY_MIN_DELAY` / `FEEDBACK_RETRY_MAX_DELAY` | `10s` / `15m`; positive, ordered, maximum 15 minutes |
| `FEEDBACK_WORKER_POLL_INTERVAL` | `1s`; range `100ms`–`1m` |
| `FEEDBACK_SMTP_HOST` / `FEEDBACK_SMTP_PORT` | Required server and port 1–65535 |
| `FEEDBACK_SMTP_TLS_MODE` | Required `starttls` or `tls`; verified TLS, no plaintext fallback |
| `FEEDBACK_SMTP_USERNAME` / `FEEDBACK_SMTP_PASSWORD` | Required authentication |
| `FEEDBACK_MAIL_FROM` / `FEEDBACK_MAIL_TO` | Required service sender and fixed operator recipient |
| `FEEDBACK_TELEGRAM_BOT_TOKEN` / `FEEDBACK_TELEGRAM_CHAT_ID` | Required bot and notification chat |
| `FEEDBACK_TELEGRAM_PROXY_URL` | Required private HTTP proxy URL with explicit port and no embedded credentials |
| `FEEDBACK_TELEGRAM_PROXY_USERNAME` / `FEEDBACK_TELEGRAM_PROXY_PASSWORD` | Required separate proxy authentication; each value at most 256 bytes, username without `:` |

Credentials and mailbox/chat values can use their corresponding `_FILE` variables instead of the base variable. Set only one form. Files must be closed regular files, mode `0400` or `0600`, readable by the process, and at most 4096 bytes. A trailing newline is removed. The subscription is not feedback configuration; only the VPN container receives it.

Configuration errors identify parameter names and do not print values. Do not put credentials in repository files, command arguments, images, or captured Compose output. The earlier secure forms are outside this checkout and are not automatically loaded or deployed.

SMTP connects within five seconds and limits the complete session to fifteen seconds. Its plain-text UTF-8 MIME contains the request; `Reply-To` uses the validated form address. `From`, `To`, and the stable `Message-ID` domain come from configuration. Success means SMTP DATA acceptance. Telegram has a dedicated fifteen-second HTTP client, fixed Bot API origin, verified TLS through CONNECT, separate proxy authentication, and no redirects, ambient proxy lookup, or direct fallback. Provider bodies, token URLs, contacts, IP, and User-Agent are not logged or journaled as delivery errors.

SMTP authentication uses the server's capabilities after verified TLS. It prefers advertised `PLAIN` and otherwise supports advertised `LOGIN`. Unsupported authentication stops before credentials are sent. An authentication rejection does not trigger another mechanism or a second login attempt in the same delivery. `LOGIN` requires TLS, the configured server identity, and the expected username/password challenge sequence. Set the SMTP username to the full mailbox address when the provider requires it; the sender must be a valid mailbox address, with any display name kept separate from the address.

Each channel has one worker. Transient errors use exponential delay with jitter; Telegram `retry_after` can extend the delay. Expired or exhausted jobs become `failed`. An explicit administrative retry starts a new bounded cycle without changing intake time or retention. A failed confirmation write stops the worker; it cannot keep sending an unconfirmed job.

## Journal and operations

One versioned append-only JSONL file holds acceptance, both initial jobs, and later delivery state. Serialized acceptance performs identity comparison, append, `fsync`, and index publication. Files use `0600`. A permanent adjacent `feedback.lock` uses exclusive `flock`; a second process or administrative command cannot enter a running store.

Recovery preserves complete valid records, truncates only an incomplete final line, and rejects corrupt complete records, unknown versions, and impossible transitions. Recovered in-flight work keeps attempts and retry age. Unconfirmed append rollback disables intake and delivery until recovery. Abandoned compaction candidates are removed under the lock so later physical deletion does not leave stale snapshots.

Retention runs at startup and every 24 hours. Deletion removes requests and jobs through a closed temporary snapshot, file `fsync`, atomic rename, directory `fsync`, and reopening the current journal. Admission conservatively reserves capacity for bounded future state events and the compaction copy; actual usable intake space is below the configured maximum. The filesystem must also have that physical space available. Full intake capacity returns `503` without discarding accepted jobs. A provider request already dispatched during retention can finish, but the removed request is not recreated. External mail and Telegram copies are outside journal retention.

Build the administrative binary with `go build -o <owned-output>/feedback ./cmd/feedback`. In the application image it is `/feedback`. All commands take the same storage configuration and exclusive lock; stop the server first. They do not need SMTP or bot credentials.

```text
feedback export [--since YYYY-MM-DD]
feedback delete --email address@example.org
feedback purge
feedback status
feedback retry --submission-id <uuid-v4> --channel smtp|telegram
```

`export` emits JSONL with personal data; protect its output. Dates start at midnight in Europe/Moscow. `delete` matches the normalized address and preserves local-part case. `purge` removes records older than configured retention. `status` emits job IDs, attempts, times, and safe codes without form data. `retry` queues only a failed channel and sends nothing itself.

`GET`/`HEAD /health` and `/healthz` confirm process responsiveness. `GET`/`HEAD /readyz` reflect journal availability, with `503` if unavailable. Provider and VPN outages do not change process health or journal readiness. `server healthcheck` is the container process check. SIGINT/SIGTERM stop intake, cancel worker waits and provider calls, persist recoverable outcomes, and join all owned goroutines; HTTP shutdown has a ten-second deadline.

## Containers and Telegram VPN

`Dockerfile` builds a non-root scratch server and administrative CLI. `Dockerfile.vpn` builds the updater around official Xray `26.9.8` with pinned multi-architecture digest `sha256:3629bf7d825748cda29698ac354f8f2146f6b292edb9eb0c7cb7fe0583dae091`. Its `config-check` target validates synthetic configurations and exercises actual CONNECT routing with a dummy VLESS listener under `--network=none`. Optional WS/gRPC fixtures produce informational deprecation warnings from the pinned core; the supplied subscription shape uses TCP/REALITY.

`compose.yaml` has private feedback and VPN containers, separate persistent volumes, read-only roots, dropped capabilities, and no host ports. `telegram-link` is internal and joins only those two services. VPN has a separate uplink; feedback keeps its own application/SMTP uplink. Factory must attach gateway to the feedback application network when it integrates the candidate assembly. Feedback startup does not wait for VPN health.

Set `FEEDBACK_IMAGE` and `TELEGRAM_VPN_IMAGE` to tested `sha-<full-commit-SHA>` application images. `FEEDBACK_COMMIT` supplies OCI revision metadata. Set `FEEDBACK_ENV_FILE` to the closed external mail/bot and public service configuration file. The three Compose secret sources are `TELEGRAM_VPN_SUBSCRIPTION_URL_FILE`, `TELEGRAM_PROXY_USERNAME_FILE`, and `TELEGRAM_PROXY_PASSWORD_FILE`. They contain a raw URL, username, and password, respectively; the earlier dotenv subscription file is not itself a raw URL secret file.

Provision fresh random proxy credentials separately from the bot and node credentials. Only feedback and VPN receive those two files. Only VPN receives the subscription URL; it receives no mail or bot secrets. Local Compose can bind secret files without applying requested UID/GID/mode, so provision the source files as UID/GID `65532:65532`, mode `0400` or `0600`, on the Linux runtime host. Named state volumes copy the closed directory from each image on first creation. Existing volumes must already have the correct ownership and `0700` directory mode.

Set `TELEGRAM_VPN_DNS` to the controlled resolver used for subscription and VPN-node bootstrap. This is direct VPN-container control traffic. It does not enable direct Telegram egress in the application. The Telegram application transport always uses the fixed proxy.

The importer accepts a bounded HTTPS text or base64 list of VLESS URIs. It rejects unsupported or misplaced parameters and does not import arbitrary listeners, routing, DNS, scripts, or logs. Supported synthetic configuration tests cover TCP, WebSocket, gRPC, XHTTP, TLS, and REALITY. The supplied subscription was checked only for sanitized parameter shape: eight TCP/REALITY nodes, with optional short ID. No live tunnel or notification was tested.

The generated inbound requires proxy authentication. Routing allows only TCP `full:api.telegram.org` at port 443 through an imported VLESS outbound. Domain strategy is `AsIs`; the first outbound and the catch-all are blackhole. There is no direct outbound. The default selected node is the first valid URI; a validated update can change selection by changing the order of the supplied list. No automatic direct fallback exists.

Refresh defaults to 24 hours. A failed refresh keeps the last validated closed config for at most 48 hours; expiry closes the VPN child. The updater pauses the child during its bounded refresh and validates the exact core before atomic installation. It restarts a fresh last-known-good config after a failed update; queued Telegram work resumes afterward. SMTP and intake remain available during that pause. `telegram-vpn healthcheck` checks the local listener, not remote Telegram availability. `telegram-vpn status` shows only cache freshness and Moscow timestamps. Docker restarts on process exit; an unhealthy check alone does not restart a container.

## Quality and verification

Pinned tools: Go 1.26.9, golangci-lint 2.14.0, go-arch-lint 1.19.0. Install exact quality tools with `make tools`. They are not production dependencies.

- `make check`: module inventory, non-mutating gofmt/goimports, build, lint, architecture, and fresh race tests.
- `make quality-test`: positive and deliberately invalid temporary projects that verify enforcement.
- `make fmt FILES="path.go"`: format explicit files; `fmt-all` is for a separate formatting change.
- `go mod tidy` and `go mod verify`: dependency consistency and integrity.
- `docker build --target config-check -f Dockerfile.vpn .`: exact-core synthetic configuration validation.
- `docker build -f Dockerfile .` and `docker build -f Dockerfile.vpn .`: application and VPN images.
- `bash tests/container/feedback-smoke.sh <amd64-feedback-image> <amd64-vpn-image>`: disposable local container checks, synthetic intake, permissions, provider/VPN outage isolation, and safe logs. This test alone exposes a temporary loopback port and cleans its own containers and bind fixtures.

Service checks use `GOWORK=off`. Unknown packages and nested modules fail. Keep `GOPATH`, `GOMODCACHE`, `GOCACHE`, and `GOLANGCI_LINT_CACHE` outside the checkout so downloaded source is not scanned as service code. `.go-quality-modules` declares the single root module. Production must not import fixtures, factory, or other service modules; core packages cannot read clocks or depend on adapters.

Local tests use synthetic SMTP, CONNECT, Bot API, VLESS, and filesystem data. They cover duplicate concurrent acceptance, consent replay, independent channels, terminal-state restart, provider failure, strict parsing, retention, rollback, capacity, and lock behavior. CI runs pinned checks, container builds, and the synthetic runtime smoke on pushes and pull requests.

## Tasks and compatibility

- [Feedback intake and notifications](https://github.com/JustIkra/umir-feedback/issues/1)
- [Gateway routing and public guards](https://github.com/JustIkra/umir-gateway/issues/10)
- [Registration challenges and Figma email](https://github.com/JustIkra/umir-feedback/issues/2)
- [Gateway registration confirmation](https://github.com/JustIkra/umir-gateway/issues/13)

Registration challenges and their email template are not implemented here. Feedback will own codes and delivery; gateway will keep operator activation and sessions.

The HTTP inputs/results, journal format, configuration, CLI, and retry/retention behavior are compatibility surfaces. The repository uses SemVer; before 1.0, additions or incompatible changes increment minor and compatible fixes increment patch. Breaking changes need migration notes. No release or tag exists. Future application image identities remain `sha-<full-commit-SHA>` with OCI source/revision metadata; factory selects exact tested component revisions.

Transport references: [Telegram Bot API](https://core.telegram.org/bots/api#sendmessage), [Go SMTP](https://pkg.go.dev/net/smtp), [Go HTTP transport](https://pkg.go.dev/net/http#Transport), [Xray routing](https://xtls.github.io/en/config/routing.html), and [Compose secrets](https://docs.docker.com/reference/compose-file/secrets/).
