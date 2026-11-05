#!/usr/bin/env sh
set -eu

flow_dev_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
flow_repo_dir=$(CDPATH= cd -- "$flow_dev_dir/.." && pwd)
flow_secret_dir="$flow_repo_dir/deploy/secrets"
flow_keyring="$flow_secret_dir/flow-credential-keyring.json"

if [ -e "$flow_keyring" ]; then
	printf 'Credential keyring already exists: %s\n' "$flow_keyring"
	exit 0
fi

if ! command -v openssl >/dev/null 2>&1; then
	printf 'openssl is required to generate the credential keyring\n' >&2
	exit 1
fi

umask 077
mkdir -p "$flow_secret_dir"
flow_key=$(openssl rand -base64 32 | tr -d '\n')
flow_partial="$flow_keyring.part"
trap 'unlink "$flow_partial" 2>/dev/null || true' EXIT HUP INT TERM
printf '{"activeVersion":1,"keys":{"1":"%s"}}\n' "$flow_key" > "$flow_partial"
mv "$flow_partial" "$flow_keyring"
trap - EXIT HUP INT TERM

printf 'Created credential keyring: %s\n' "$flow_keyring"
