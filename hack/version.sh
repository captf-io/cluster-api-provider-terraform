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

# Prints the version stamped into every binary and image (Makefile VERSION,
# k8s.io/component-base/version.gitVersion). It is always a valid semantic
# version, because component-base's metrics registry parses it at startup
# and panics on anything else:
#
#   at a release tag          v0.1.0
#   commits after a tag       v0.1.0-3-g1a2b3c4d5e6f[-dirty]  (git describe)
#   no release tag yet        v0.0.0-dev.g1a2b3c4d5e6f[.dirty]
#   not a git checkout        v0.0.0-dev
#
# The untagged form puts a "g" before the commit so the identifier is never
# all digits: an all-digit identifier with a leading zero (a hash such as
# 012345678901) is not valid semver. Tested by hack/version_test.sh.
set -euo pipefail

# Semantic Versioning 2.0.0 with a leading "v": no leading zeros in the
# version numbers or in numeric prerelease identifiers.
num='(0|[1-9][0-9]*)'
ident='([0-9]*[A-Za-z-][0-9A-Za-z-]*|0|[1-9][0-9]*)'
semver="^v${num}\.${num}\.${num}(-${ident}(\.${ident})*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$"

if ! head="$(git rev-parse --short=12 HEAD 2>/dev/null)"; then
	echo "v0.0.0-dev"
	exit 0
fi

if v="$(git describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=12 --dirty 2>/dev/null)" &&
	[[ "${v}" =~ ${semver} ]]; then
	echo "${v}"
	exit 0
fi

v="v0.0.0-dev.g${head}"
if ! git diff --quiet HEAD 2>/dev/null; then
	v="${v}.dirty"
fi
echo "${v}"
