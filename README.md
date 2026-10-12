# Umir Feedback

Independent Go service for durable feedback intake and two notification channels. SMTP sends the full request to one configured operator address. Telegram sends only the submission ID, Moscow time, and a reminder to check that mailbox. Telegram uses a separate private Xray CONNECT container.

The service has no published release. Gateway feedback routing and the optional factory candidate are integrated in their source repositories. This branch contains the registration challenge candidate originally prepared for feedback #2. The approved [auth extraction](https://github.com/JustIkra/umir-gateway/issues/18) now assigns account/session, OTP and auth mail to a separate private auth service. The candidate remains disabled for coordinated transfer or reuse. No real provider notification was sent during implementation.

## Architecture

Dependencies point toward business rules:

- `internal/domain` owns feedback and challenge values, validation, identifiers, and retry limits.
- `internal/application` owns intake, content fingerprints, consent, delivery decisions, and serialized challenge transitions. It defines its repository, sender, clock, normalization, jitter, and secret-mechanism ports.
- `internal/adapters/journal` implements one durable JSONL journal and its exclusive lock. SMTP, Telegram, HTTP, runtime configuration, and VPN mechanisms are separate adapters.
- `bootstrap` wires implementations and owns the HTTP listener, form workers, optional code-mail worker, retention, cancellation, and joins.
- `cmd/server`, `cmd/feedback`, and `cmd/telegram-vpn` are process entry points. Tests and synthetic transport data stay outside production mechanisms.

The service does not import other service modules. Gateway owns public routing and guards; its current implementation still stores accounts and sessions until auth extraction #18. The approved target gives private auth one account/session/OTP/mail owner. Feedback retains stored form requests and their SMTP/Telegram notification jobs. SMTP and Telegram failures do not block durable intake. A journal failure stops delivery and makes intake unavailable.

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

## Registration email challenges

OTP is disabled by default. The private protocol below is the candidate originally specified by feedback #2 and gateway #13; auth extraction #18 supersedes their intended service ownership. Transfer or reuse the implementation with one challenge/proof/outbox owner before production integration. Do not enable competing OTP paths in feedback and auth. After the owner switches, remove replaced code rather than retaining a second mechanism.

For isolated candidate acceptance, `FEEDBACK_OTP_ENABLED=true` requires every configured key and operator detail. Incomplete enabled configuration fails startup. Leave it disabled to keep separately configured feedback intake available. The same private listener serves the internal routes below; no host port or public gateway/Nginx mapping is added. The future auth owner must control the configured target, pending registration, credential verification, activation and sessions, while gateway exposes the approved public boundary.

Each call needs exactly one `Authorization: Bearer` header with `FEEDBACK_SERVICE_TOKEN`. The service compares its token digest in constant time. It accepts only purpose `operator-registration`, strict JSON of at most 8 KiB, exact field names, canonical six-digit codes, and UUID v4 operation IDs. It rejects recipient names/lists, query parameters, caller-supplied content, malformed text, null or duplicate fields, and unknown fields. Only the email domain becomes lowercase; local-part case remains unchanged.

| Private operation | Required JSON fields | Success |
| --- | --- | --- |
| `POST /internal/v1/email-challenges` | `registrationID`, `email`, `purpose` | `202 {challengeID, expiresAt, resendAvailableAt}` |
| `POST /internal/v1/email-challenges/{challengeID}/resend` | `registrationID`, `purpose`, `issuanceID` | `200` with the issuance receipt |
| `POST /internal/v1/email-challenges/{challengeID}/verify` | `registrationID`, `purpose`, `verificationID`, `code` | `200 {proofID, verifiedAt}` |
| `POST /internal/v1/email-challenges/{challengeID}/complete` | `registrationID`, `purpose`, `completionID`, `proofID` | `200 {completedAt}` |

Begin stores the challenge and its first SMTP job in one fsynced record before acknowledgement. Exact begin replay returns the original receipt without another job. A resend uses a new issuance ID for a new button action and reuses that ID after an uncertain response. It sends the same code with the original deadline. A new guess uses a new verification ID; retry the same ID and code after an uncertain response. Matching wrong-code replay returns the same `400 InvalidCode` without counting another guess. Successful verification consumes the code and persists an encrypted recoverable proof. Complete requires that proof and a stable completion ID. Neither operation creates an account or session.

Codes contain six unbiased CSPRNG digits, including leading zeros, and expire ten minutes after creation. Resends have a 60-second cooldown and at most three accepted resends per challenge. Target creation is limited to three per rolling ten minutes. Five wrong unique verification IDs lock the challenge. Process guards run before lookup and limit issue/resend calls to 60 per ten minutes and verify calls to 300 per minute. These process counters are bounded and reset at process restart; target/challenge counters come from retained records. Unknown IDs do not allocate keyed state. Capacity defaults to 4096 retained challenges and is also bounded by the shared journal byte envelope; it can be exhausted before the count limit.

The service returns fixed JSON errors `{code, message, requestID}` with `Cache-Control: no-store`: `400 InvalidRequest` or `InvalidCode`, `401 Unauthorized`, `404 ChallengeUnavailable`, `409 ChallengeIdentityConflict`, `VerificationIdentityConflict` or `CompletionIdentityConflict`, `429 ChallengeRateLimited` with `Retry-After`, and `503 FeedbackStorageUnavailable` or `FeedbackOutcomeUnknown`. Wrong methods, media types and oversized private bodies use `400 InvalidRequest`. A storage result with an unknown commit never becomes success; the challenge owner stays unavailable until restart. Gateway must translate private errors into its public registration contract.

Begin bindings survive for 24 hours after creation. A successful verification extends retention to exactly 24 hours after verification; matching successful retries return the same proof through this recovery window, including after OTP expiry or restart. Complete retries return the original completion result in this window. New verification IDs cannot reuse a consumed challenge. After the window closes, the service removes the record; gateway must start a new permitted challenge. Recovery and requests check deadlines strictly. Physical cleanup runs at startup and on worker polls, including when OTP is disabled; its interval is bounded by `FEEDBACK_WORKER_POLL_INTERVAL`.

The journal stores HMAC-SHA-256 code/attempt and proof digests. Code and recoverable proof use AES-256-GCM with fresh 96-bit nonces and challenge/purpose/type-bound AAD. Code ciphertext is physically removed through compaction at verification, lock or expiry. Retained proof ciphertext is removed when the recovery window ends. Lost-response replay needs the same keys across restart. Do not replace keys while retained challenges exist. A missing, invalid or mismatched key fails enabled startup; there is no plaintext fallback. Code, proof, recipient, payload, auth header, and secret values are not written to service logs or Telegram.

OTP has a separate SMTP worker. Each issuance allows at most three attempts, independently of the form retry policy. Provider downtime leaves a durably accepted job queued. SMTP timeout or crash can duplicate the same issuance, code and stable Message-ID. Successful verification cancels unsent/in-flight jobs. Delivery is bounded by the original expiry and retries stop at expiry. SMTP uses the existing verified TLS/STARTTLS transport; OTP never uses Telegram VPN or the form's fixed operator recipient.

The current Go SMTP adapter uses PLAIN authentication. The supplied provider binding has been verified separately with LOGIN, without recipient or DATA dispatch. Those checks do not establish PLAIN compatibility or delivery by this adapter. Auth extraction #18 must check or adapt the mechanism before production enablement; local SMTP mock evidence does not resolve that provider check.

| OTP variable | Requirement |
| --- | --- |
| `FEEDBACK_OTP_ENABLED` | `false` by default; exact `true` enables the feature |
| `FEEDBACK_OTP_MAX_CHALLENGES` | `4096`; integer 1–4096 |
| `FEEDBACK_SERVICE_TOKEN` | Independent high-entropy shared secret, strict unpadded base64url encoding of 32–128 random bytes; only gateway and feedback receive it |
| `FEEDBACK_OTP_PEPPER` | Standard base64 encoding of 32 random bytes; feedback only |
| `FEEDBACK_OTP_ENCRYPTION_KEY` | Standard base64 encoding of a different 32-byte random value; feedback only |
| `FEEDBACK_OPERATOR_NAME` | Required real operator name; at most 256 UTF-8 bytes |
| `FEEDBACK_OPERATOR_ADDRESS` | Required real postal address; at most 512 UTF-8 bytes |
| `FEEDBACK_REGISTRATION_SENDER_NAME` | Required display name in From; at most 160 UTF-8 bytes; From address uses `FEEDBACK_MAIL_FROM` |
| `FEEDBACK_PRIVACY_URL` | Optional fixed HTTPS policy URL; no credentials, query, fragment or tracking |

The three secret variables support their `_FILE` forms. Apply the same closed-file rules as provider credentials. Do not put real keys in examples, source, command arguments or images. Operator details must have no control characters or placeholder text. Generate the three secrets separately with a cryptographic random source during runtime provisioning; preserve them on the private host. Enabling this feature does not automatically load the earlier SMTP/bot secrets or send a real test email.

The email adapts [Figma 453:771 and 453:795](https://www.figma.com/design/6Y1Jglyvc1xVsTzII54A2u/UmirHack?node-id=453-770) to registration. It uses one-column tables and inline CSS, a 600-pixel maximum width, the exact 2× PNG mark embedded by CID, a hidden preheader, and multipart/alternative HTML/plain text. No web font, external image, script, tracking pixel, code link or confirmation button is required. The code remains selectable text grouped by three digits. Request time and the original expiry use Moscow time; resend copy does not claim a fresh ten-minute period.

Synthetic previews and a preview generator are in `tests/email`. Browser renders cover 600, 390 and 320 pixels, plus an oversized-footer/missing-image case. These establish browser layout only. Actual Gmail web/mobile, Outlook desktop, Apple Mail and light/dark email-client rendering remain an acceptance item before production enablement. Local TLS SMTP mock tests cover actual MIME, fixed recipient, CID and deadline behavior; no real mailbox was used.

Run `go run ./tests/email` from the repository root to regenerate the ignored synthetic HTML and plain-text examples. Open the HTML with the available browser-control tools; do not launch macOS Chrome from a sandboxed shell. Preview output is not a production template or real notification.

For the optional secret mounts, use `docker compose -f compose.yaml -f compose.otp.yaml config --quiet`, then the normal reviewed deployment procedure. Set the three host `_FILE` variables to closed raw secret files. The overlay enables OTP and mounts only those files into feedback; operator details remain in the external feedback env file. It adds no port, network, image pin or public route. Local Compose bind mounts may ignore requested UID/mode; prepare runtime files as `65532:65532` with mode `0400` or `0600`.

## Journal and operations

One versioned append-only JSONL file holds acceptance, both initial jobs, and later delivery state. Serialized acceptance performs identity comparison, append, `fsync`, and index publication. Files use `0600`. A permanent adjacent `feedback.lock` uses exclusive `flock`; a second process or administrative command cannot enter a running store.

Recovery preserves complete valid records, truncates only an incomplete final line, and rejects corrupt complete records, unknown versions, and impossible transitions. Recovered in-flight work keeps attempts and retry age. Unconfirmed append rollback disables intake and delivery until recovery. Abandoned compaction candidates are removed under the lock so later physical deletion does not leave stale snapshots. New challenge records share version 1 and the same lock; their snapshots have strict bounded fields and an 8 KiB maximum. After an OTP record is written, old binaries without challenge support cannot read that journal. Keep a compatible reader for rollback; do not delete accepted records to downgrade.

Retention runs at startup and every 24 hours. Deletion removes requests and jobs through a closed temporary snapshot, file `fsync`, atomic rename, directory `fsync`, and reopening the current journal. Admission conservatively reserves capacity for bounded future state events and the compaction copy; actual usable intake space is below the configured maximum. The filesystem must also have that physical space available. Full intake capacity returns `503` without discarding accepted jobs. A provider request already dispatched during retention can finish, but the removed request is not recreated. External mail and Telegram copies are outside journal retention.

Build the administrative binary with `go build -o <owned-output>/feedback ./cmd/feedback`. In the application image it is `/feedback`. All commands take the same storage configuration and exclusive lock; stop the server first. They do not need SMTP or bot credentials.

```text
feedback export [--since YYYY-MM-DD]
feedback delete --email address@example.org
feedback purge
feedback status
feedback retry --submission-id <uuid-v4> --channel smtp|telegram
```

`export` emits feedback JSONL with personal data; protect its output. Dates start at midnight in Europe/Moscow. `delete` matches the normalized form address and preserves local-part case. `purge` removes feedback records older than configured retention. These commands preserve challenge replay records until their protocol retention expires; opening the store also prunes expired challenges. `status` emits form job IDs, attempts, times, and safe codes without form data. `retry` queues only a failed form channel and sends nothing itself. There is no administrative OTP/proof export or unrestricted mail command.

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
- `docker build --target otp-smoke-probe -f Dockerfile .`: separate synthetic OTP probe image; the probe is absent from the default runtime image.
- `bash tests/container/otp-smoke.sh <feedback-image> <otp-probe-image>`: owned offline containers, private auth/protocol, journal scrubbing, provider outage, application restart, proof/completion replay, permissions and safe logs. It publishes no host port and deletes only its fixture containers and anonymous volumes.

Service checks use `GOWORK=off`. Unknown packages and nested modules fail. Keep `GOPATH`, `GOMODCACHE`, `GOCACHE`, and `GOLANGCI_LINT_CACHE` outside the checkout so downloaded source is not scanned as service code. `.go-quality-modules` declares the single root module. Production must not import fixtures, factory, or other service modules; core packages cannot read clocks or depend on adapters.

Local tests use synthetic SMTP, CONNECT, Bot API, VLESS, and filesystem data. They cover duplicate concurrent acceptance, consent replay, independent channels, terminal-state restart, provider failure, strict parsing, retention, rollback, capacity, and lock behavior. CI runs pinned checks, container builds, and the synthetic runtime smoke on pushes and pull requests.

## Tasks and compatibility

- [Feedback intake and notifications](https://github.com/JustIkra/umir-feedback/issues/1)
- [Gateway routing and public guards](https://github.com/JustIkra/umir-gateway/issues/10)
- [Registration challenges and Figma email](https://github.com/JustIkra/umir-feedback/issues/2)
- [Gateway registration confirmation](https://github.com/JustIkra/umir-gateway/issues/13)
- [Auth extraction and single OTP/mail owner](https://github.com/JustIkra/umir-gateway/issues/18)

Private registration challenges and their mail template are implemented as a disabled transfer candidate. Auth extraction #18 owns the coordinated migration, performed by JustIkra. Actual email-client renders, provider-mechanism compatibility, coordinated enrollment acceptance and production enablement remain pending. Factory's private auth mail configuration holds the owner's supplied footer/sender details. [Frontend issue #1](https://github.com/JustIkra/umir-frontend/issues/1#issuecomment-6125481749) tracks the future public `/privacy` page and fixed mail link; page availability and approved policy text remain frontend work.

The HTTP inputs/results, journal format, configuration, CLI, and retry/retention behavior are compatibility surfaces. The repository uses SemVer; before 1.0, additions or incompatible changes increment minor and compatible fixes increment patch. Breaking changes need migration notes. No release or tag exists. Future application image identities remain `sha-<full-commit-SHA>` with OCI source/revision metadata; factory selects exact tested component revisions.

Transport references: [Telegram Bot API](https://core.telegram.org/bots/api#sendmessage), [Go SMTP](https://pkg.go.dev/net/smtp), [Go HTTP transport](https://pkg.go.dev/net/http#Transport), [Xray routing](https://xtls.github.io/en/config/routing.html), and [Compose secrets](https://docs.docker.com/reference/compose-file/secrets/).
