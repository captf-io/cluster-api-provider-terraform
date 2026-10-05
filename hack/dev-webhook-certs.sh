#!/usr/bin/env bash
# Copyright 2026 The CAPTF Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

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
