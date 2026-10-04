#!/usr/bin/env bash
# Creates a self-signed webhook serving certificate (tls.crt, tls.key) for
# localhost in the given directory, unless both files already exist. Only for
# `make run`: in a cluster, cert-manager issues the certificate
# (config/certmanager). The key is written with mode 0600.
# Usage: hack/dev-webhook-certs.sh <dir>
set -euo pipefail

if [[ $# -ne 1 ]]; then
	echo "usage: $0 <dir>" >&2
	exit 2
fi
dir="$1"

if [[ -s "${dir}/tls.crt" && -s "${dir}/tls.key" ]]; then
	exit 0
fi

mkdir -p "${dir}"
umask 077
openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
	-subj "/CN=localhost" \
	-addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
	-keyout "${dir}/tls.key" -out "${dir}/tls.crt" 2>/dev/null
echo "dev-webhook-certs: wrote ${dir}/tls.crt and ${dir}/tls.key"
