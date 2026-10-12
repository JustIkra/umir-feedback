#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
	printf 'usage: %s <feedback-image> <telegram-vpn-image>\n' "$0" >&2
	exit 2
fi

feedback_image=$1
telegram_vpn_image=$2
workdir=$(mktemp -d)
app_name="umir-feedback-test-app-$$"
vpn_name="umir-feedback-test-vpn-$$"
copyup_name="umir-feedback-test-copyup-$$"
app_id=""
vpn_id=""
copyup_id=""
cleanup() {
	if [[ -n "$app_id" ]] && docker inspect "$app_id" >/dev/null 2>&1; then
		docker stop "$app_id" >/dev/null
	fi
	if [[ -n "$vpn_id" ]] && docker inspect "$vpn_id" >/dev/null 2>&1; then
		docker stop "$vpn_id" >/dev/null
	fi
	if [[ -n "$copyup_id" ]] && docker inspect "$copyup_id" >/dev/null 2>&1; then
		docker stop "$copyup_id" >/dev/null
	fi
	if [[ -d "$workdir" ]]; then
		docker run --rm --platform linux/amd64 --network none --user 65532:65532 \
			--mount "type=bind,src=$workdir,dst=/fixture" \
			golang:1.26.9-alpine sh -ceu 'rm -rf /fixture/app-state /fixture/vpn-state /fixture/secrets /fixture/app.env /fixture/request.json'
	fi
	rm -rf "$workdir"
}
trap cleanup EXIT
chmod 0777 "$workdir"

docker run --rm --platform linux/amd64 --user 65532:65532 \
	--mount "type=bind,src=$workdir,dst=/fixture" \
	golang:1.26.9-alpine sh -ceu '
mkdir -m 0700 /fixture/app-state
mkdir -m 0700 /fixture/vpn-state /fixture/secrets
cat > /fixture/app.env <<"ENV"
FEEDBACK_LISTEN_ADDRESS=0.0.0.0:8084
FEEDBACK_STORE_PATH=/var/lib/umir-feedback/journal.jsonl
FEEDBACK_MAX_STORE_BYTES=1048576
FEEDBACK_RETENTION_DAYS=30
FEEDBACK_CONSENT_VERSION=container-smoke-v1
FEEDBACK_SMTP_HOST=127.0.0.1
FEEDBACK_SMTP_PORT=25
FEEDBACK_SMTP_TLS_MODE=starttls
FEEDBACK_SMTP_USERNAME=synthetic-smtp-user
FEEDBACK_SMTP_PASSWORD=synthetic-smtp-password
FEEDBACK_MAIL_FROM=feedback@example.invalid
FEEDBACK_MAIL_TO=ops@example.invalid
FEEDBACK_TELEGRAM_BOT_TOKEN=123456:synthetic-test-token
FEEDBACK_TELEGRAM_CHAT_ID=123456
FEEDBACK_TELEGRAM_PROXY_URL=http://127.0.0.1:8080
FEEDBACK_TELEGRAM_PROXY_USERNAME=synthetic-proxy-user
FEEDBACK_TELEGRAM_PROXY_PASSWORD=synthetic-proxy-password
ENV
chmod 0400 /fixture/app.env
cat > /fixture/request.json <<"JSON"
{"submissionID":"00000000-0000-4000-8000-000000000001","name":"Synthetic Container Test","email":"synthetic@example.invalid","phone":"+15555550100","message":"Disposable container acceptance","consent":{"accepted":true,"documentVersion":"container-smoke-v1"},"website":""}
JSON
chmod 0400 /fixture/request.json
printf "%s\n" "https://subscription.invalid/vless" > /fixture/secrets/subscription-url
printf "%s\n" "synthetic-proxy-user" > /fixture/secrets/proxy-username
printf "%s\n" "synthetic-proxy-password" > /fixture/secrets/proxy-password
chmod 0400 /fixture/secrets/*
'

app_id=$(docker run --rm --detach --platform linux/amd64 \
	--name "$app_name" \
	--user 65532:65532 \
	--read-only \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m,uid=65532,gid=65532,mode=0700 \
	--publish 127.0.0.1::8084 \
	--mount "type=bind,src=$workdir/app-state,dst=/var/lib/umir-feedback" \
	--env-file "$workdir/app.env" \
	"$feedback_image")

host_port=$(docker port "$app_id" 8084/tcp | awk -F: '{print $NF}')
base_url="http://127.0.0.1:$host_port"
ready=false
for attempt in {1..30}; do
	if curl --silent --fail "$base_url/readyz" >/dev/null; then
		ready=true
		break
	fi
	sleep 1
done
if [[ "$ready" != true ]]; then
	docker inspect --format '{{.State.Status}} {{.State.Health.Status}}' "$app_id" >&2
	exit 1
fi

if [[ $(docker inspect --format '{{.Config.User}}' "$app_id") != "65532:65532" ]]; then
	printf 'feedback container did not run as UID/GID 65532\n' >&2
	exit 1
fi
healthy=false
for attempt in {1..30}; do
	if [[ $(docker inspect --format '{{.State.Health.Status}}' "$app_id") == "healthy" ]]; then
		healthy=true
		break
	fi
	sleep 1
done
if [[ "$healthy" != true ]]; then
	printf 'feedback healthcheck did not pass\n' >&2
	exit 1
fi

status=$(curl --silent --show-error --output "$workdir/response.json" --write-out '%{http_code}' \
	--header 'Content-Type: application/json' \
	--data-binary "@$workdir/request.json" \
	"$base_url/feedback")
if [[ "$status" != 201 ]]; then
	printf 'feedback intake returned HTTP %s\n' "$status" >&2
	exit 1
fi

if ! docker run --rm --platform linux/amd64 --user 65532:65532 \
	--mount "type=bind,src=$workdir/app-state,dst=/state,readonly" \
	golang:1.26.9-alpine sh -ceu '
test "$(stat -c "%u:%g:%a" /state)" = "65532:65532:700"
test "$(stat -c "%u:%g:%a" /state/journal.jsonl)" = "65532:65532:600"
'
then
	printf 'feedback state path permissions were not preserved\n' >&2
	exit 1
fi

if ! curl --silent --show-error --fail "$base_url/readyz" >/dev/null; then
	printf 'feedback readiness changed while providers were unavailable\n' >&2
	exit 1
fi

if docker logs "$app_id" 2>&1 | grep -E 'synthetic-(smtp|test-token|proxy)' >/dev/null; then
	printf 'synthetic provider credentials appeared in container logs\n' >&2
	exit 1
fi

vpn_id=$(docker run --rm --detach --platform linux/amd64 \
	--name "$vpn_name" \
	--user 65532:65532 \
	--read-only \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--health-interval 1s \
	--health-timeout 1s \
	--health-start-period 1s \
	--health-retries 2 \
	--network none \
	--mount "type=bind,src=$workdir/vpn-state,dst=/var/lib/telegram-vpn" \
	--mount "type=bind,src=$workdir/secrets/subscription-url,dst=/run/secrets/telegram-vpn-subscription-url,readonly" \
	--mount "type=bind,src=$workdir/secrets/proxy-username,dst=/run/secrets/telegram-proxy-username,readonly" \
	--mount "type=bind,src=$workdir/secrets/proxy-password,dst=/run/secrets/telegram-proxy-password,readonly" \
	--env TELEGRAM_VPN_LISTEN_PORT=8080 \
	--env TELEGRAM_VPN_REFRESH_INTERVAL=24h \
	--env TELEGRAM_VPN_MAX_LKG_AGE=48h \
	"$telegram_vpn_image")

vpn_health=starting
for attempt in {1..15}; do
	vpn_health=$(docker inspect --format '{{.State.Health.Status}}' "$vpn_id")
	if [[ "$vpn_health" == "unhealthy" ]]; then
		break
	fi
	sleep 1
done
if [[ "$vpn_health" != "unhealthy" ]]; then
	printf 'VPN without a valid subscription did not fail closed\n' >&2
	exit 1
fi
if [[ $(docker inspect --format '{{.State.Status}}' "$vpn_id") != "running" ]]; then
	printf 'VPN supervisor exited instead of keeping the channel unavailable\n' >&2
	exit 1
fi
vpn_status=$(docker exec "$vpn_id" /usr/local/bin/telegram-vpn status 2>&1 || true)
if [[ "$vpn_status" != "state=unavailable" ]]; then
	printf 'VPN status did not report an empty cache\n' >&2
	exit 1
fi
if docker logs "$vpn_id" 2>&1 | grep -E 'subscription\.invalid|synthetic-proxy' >/dev/null; then
	printf 'VPN subscription or proxy credentials appeared in logs\n' >&2
	exit 1
fi
if ! curl --silent --fail "$base_url/readyz" >/dev/null; then
	printf 'feedback readiness waited for the unavailable VPN\n' >&2
	exit 1
fi

copyup_id=$(docker run --rm --detach --platform linux/amd64 \
	--name "$copyup_name" \
	--user 65532:65532 \
	--read-only \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--network none \
	--volume /var/lib/umir-feedback \
	--env-file "$workdir/app.env" \
	"$feedback_image")

copyup_healthy=false
for attempt in {1..15}; do
	if docker exec "$copyup_id" /server healthcheck >/dev/null 2>&1; then
		copyup_healthy=true
		break
	fi
	sleep 1
done
if [[ "$copyup_healthy" != true ]]; then
	printf 'feedback did not start on Docker volume copy-up\n' >&2
	exit 1
fi

copyup_mount=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/umir-feedback"}}{{.Type}} {{.Name}}{{end}}{{end}}' "$copyup_id")
if [[ "$copyup_mount" != volume\ * ]]; then
	printf 'copy-up probe did not use an anonymous volume\n' >&2
	exit 1
fi
copyup_mode=$(docker run --rm --platform linux/amd64 --network none --user 65532:65532 \
	--volumes-from "$copyup_id":ro \
	golang:1.26.9-alpine sh -ceu 'stat -c "%u:%g:%a" /var/lib/umir-feedback')
if [[ "$copyup_mode" != "65532:65532:700" ]]; then
	printf 'Docker volume copy-up permissions were %s\n' "$copyup_mode" >&2
	exit 1
fi

printf 'container smoke passed (intake, bind permissions, volume copy-up, provider failure isolation, VPN fail-closed status)\n'
