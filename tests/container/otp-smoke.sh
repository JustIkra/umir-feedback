#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
	printf 'usage: %s <feedback-image> <otp-smoke-probe-image>\n' "$0" >&2
	exit 2
fi

feedback_image=$1
probe_image=$2
workdir=$(mktemp -d)
app_name="umir-feedback-otp-smoke-app-$$"
probe_name="umir-feedback-otp-smoke-probe-$$"
app_id=""
probe_id=""

cleanup() {
	if [[ -n "$probe_id" ]] && docker inspect "$probe_id" >/dev/null 2>&1; then
		docker rm -fv "$probe_id" >/dev/null
	fi
	if [[ -n "$app_id" ]] && docker inspect "$app_id" >/dev/null 2>&1; then
		docker rm -fv "$app_id" >/dev/null
	fi
	rm -rf "$workdir"
}
trap cleanup EXIT

mkdir -m 0700 "$workdir/secrets"
node - "$workdir" <<'NODE'
const fs = require('node:fs');
const path = require('node:path');
const root = process.argv[2];
const secrets = path.join(root, 'secrets');
const write = (name, contents, mode) => fs.writeFileSync(path.join(secrets, name), contents, { mode });
write('otp-pepper', Buffer.alloc(32, 1).toString('base64'), 0o600);
write('otp-encryption-key', Buffer.alloc(32, 2).toString('base64'), 0o600);
write('service-token', Buffer.alloc(32, 3).toString('base64url'), 0o600);
write('smtp-username', 'fixture-smtp-user', 0o600);
write('smtp-password', 'fixture-smtp-password', 0o600);
write('mail-from', 'feedback@invalid.test', 0o600);
write('mail-to', 'operator@invalid.test', 0o600);
write('telegram-token', '123456:fixture-telegram-token', 0o600);
write('telegram-chat-id', '123456', 0o600);
write('proxy-username', 'fixture-proxy-user', 0o600);
write('proxy-password', 'fixture-proxy-password', 0o600);

const env = [
  'FEEDBACK_LISTEN_ADDRESS=0.0.0.0:8084',
  'FEEDBACK_STORE_PATH=/var/lib/umir-feedback/journal.jsonl',
  'FEEDBACK_MAX_STORE_BYTES=67108864',
  'FEEDBACK_RETENTION_DAYS=30',
  'FEEDBACK_CONSENT_VERSION=otp-container-smoke-v1',
  'FEEDBACK_WORKER_POLL_INTERVAL=100ms',
  'FEEDBACK_SMTP_HOST=127.0.0.1',
  'FEEDBACK_SMTP_PORT=1',
  'FEEDBACK_SMTP_TLS_MODE=starttls',
  'FEEDBACK_SMTP_USERNAME_FILE=/run/secrets/smtp-username',
  'FEEDBACK_SMTP_PASSWORD_FILE=/run/secrets/smtp-password',
  'FEEDBACK_MAIL_FROM_FILE=/run/secrets/mail-from',
  'FEEDBACK_MAIL_TO_FILE=/run/secrets/mail-to',
  'FEEDBACK_TELEGRAM_BOT_TOKEN_FILE=/run/secrets/telegram-token',
  'FEEDBACK_TELEGRAM_CHAT_ID_FILE=/run/secrets/telegram-chat-id',
  'FEEDBACK_TELEGRAM_PROXY_URL=http://127.0.0.1:1',
  'FEEDBACK_TELEGRAM_PROXY_USERNAME_FILE=/run/secrets/proxy-username',
  'FEEDBACK_TELEGRAM_PROXY_PASSWORD_FILE=/run/secrets/proxy-password',
  'FEEDBACK_OTP_ENABLED=true',
  'FEEDBACK_OTP_MAX_CHALLENGES=64',
  'FEEDBACK_SERVICE_TOKEN_FILE=/run/secrets/service-token',
  'FEEDBACK_OTP_PEPPER_FILE=/run/secrets/otp-pepper',
  'FEEDBACK_OTP_ENCRYPTION_KEY_FILE=/run/secrets/otp-encryption-key',
  'FEEDBACK_OPERATOR_NAME=Test fixture operator',
  'FEEDBACK_OPERATOR_ADDRESS=Test fixture street 1',
  'FEEDBACK_REGISTRATION_SENDER_NAME=Umir Notifications',
].join('\n') + '\n';
fs.writeFileSync(path.join(root, 'app.env'), env, { mode: 0o600 });
NODE
chmod 0600 "$workdir/app.env"
docker run --rm --network none --user 0:0 \
	--mount "type=bind,src=$workdir,dst=/fixture" \
	golang:1.26.9-alpine sh -ceu '
chown 65532:65532 /fixture/secrets/*
chmod 0400 /fixture/secrets/*
'

secret_mounts=()
for secret in "$workdir"/secrets/*; do
	name=${secret##*/}
	secret_mounts+=(--mount "type=bind,src=$secret,dst=/run/secrets/$name,readonly")
done

app_id=$(docker create \
	--name "$app_name" \
	--user 65532:65532 \
	--read-only \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--tmpfs /tmp:rw,noexec,nosuid,nodev,size=16m,uid=65532,gid=65532,mode=0700 \
	--network none \
	--volume /var/lib/umir-feedback \
	--env-file "$workdir/app.env" \
	"${secret_mounts[@]}" \
	"$feedback_image")
docker start "$app_id" >/dev/null

app_volume=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/umir-feedback"}}{{.Name}}{{end}}{{end}}' "$app_id")
if [[ -z "$app_volume" ]]; then
	printf 'application did not receive its anonymous state volume\n' >&2
	exit 1
fi

probe_id=$(docker create \
	--name "$probe_name" \
	--user 65532:65532 \
	--read-only \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--network "container:$app_id" \
	--mount "type=volume,src=$app_volume,dst=/var/lib/umir-feedback,readonly" \
	--volume /var/lib/otp-probe \
	"$probe_image")

if [[ $(docker inspect --format '{{.Config.User}}' "$app_id") != "65532:65532" || $(docker inspect --format '{{.Config.User}}' "$probe_id") != "65532:65532" ]]; then
	printf 'application or probe container did not run as UID/GID 65532\n' >&2
	exit 1
fi
probe_volume=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/var/lib/otp-probe"}}{{.Name}}{{end}}{{end}}' "$probe_id")
if [[ -z "$probe_volume" ]]; then
	printf 'probe did not receive its anonymous recovery volume\n' >&2
	exit 1
fi

docker start -a "$probe_id"
docker restart "$app_id" >/dev/null
docker start -a "$probe_id"

docker logs "$app_id" | docker run --rm --interactive \
	--user 65532:65532 \
	--read-only \
	--cap-drop ALL \
	--security-opt no-new-privileges \
	--network none \
	--mount "type=volume,src=$probe_volume,dst=/var/lib/otp-probe,readonly" \
	"$probe_image" check-logs

printf 'OTP container smoke passed (private protocol, encrypted challenge storage, provider downtime, restart recovery, completion replay, log hygiene)\n'
