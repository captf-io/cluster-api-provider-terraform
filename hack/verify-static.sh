#!/usr/bin/env bash
# Fails unless every given binary is a statically linked ELF64 executable:
# no PT_INTERP (dynamic loader) and no PT_DYNAMIC program header.
# The runner is copied into arbitrary source images (the Job pod's init
# container), so it must not depend on any libc or loader.
#
# Reads the ELF program headers with od, so it works in the golang build
# image (which has no `file`/`readelf`) and on cross-compiled binaries.
# Usage: hack/verify-static.sh <binary>...
set -euo pipefail

if [[ $# -eq 0 ]]; then
	echo "usage: $0 <binary>..." >&2
	exit 2
fi

# read_uint <file> <offset> <bytes>: little-endian unsigned integer.
read_uint() {
	od -An -v -t "u$3" -j "$2" -N "$3" --endian=little "$1" | tr -d ' '
}

fail=0
for bin in "$@"; do
	if [[ ! -f "${bin}" ]]; then
		echo "verify-static: ${bin}: no such file" >&2
		fail=1
		continue
	fi
	magic="$(od -An -v -t x1 -N 5 "${bin}" | tr -d ' ')"
	if [[ "${magic}" != "7f454c4602" ]]; then
		echo "verify-static: ${bin}: not an ELF64 binary" >&2
		fail=1
		continue
	fi
	if [[ "$(od -An -v -t u1 -j 5 -N 1 "${bin}" | tr -d ' ')" != 1 ]]; then
		echo "verify-static: ${bin}: not little-endian" >&2
		fail=1
		continue
	fi
	phoff="$(read_uint "${bin}" 32 8)"
	phentsize="$(read_uint "${bin}" 54 2)"
	phnum="$(read_uint "${bin}" 56 2)"
	dynamic=""
	for ((i = 0; i < phnum; i++)); do
		ptype="$(read_uint "${bin}" $((phoff + i * phentsize)) 4)"
		case "${ptype}" in
		2) dynamic+=" PT_DYNAMIC" ;;
		3) dynamic+=" PT_INTERP" ;;
		esac
	done
	if [[ -n "${dynamic}" ]]; then
		echo "verify-static: ${bin}: dynamically linked (${dynamic# })" >&2
		fail=1
		continue
	fi
	echo "verify-static: ${bin}: statically linked"
done

exit "${fail}"
