#!/bin/sh
#
# vector installer.
#
#   curl -fsSL https://raw.githubusercontent.com/3zequiel3/vector/main/install.sh | sh
#
# Environment:
#   VECTOR_VERSION      install this tag instead of the latest (e.g. v1.2.3)
#   VECTOR_INSTALL_DIR  install here instead of ~/.local/bin
#
# This script never uses sudo and never writes outside the install directory.

set -eu

REPO='3zequiel3/vector'
BIN='vector'

# ---------------------------------------------------------------------------
# output
# ---------------------------------------------------------------------------

log() { printf '%s\n' "$1"; }
warn() { printf '%s\n' "$1" >&2; }
die() {
	printf 'install: %s\n' "$1" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# workspace
#
# Created before anything can fail, so the trap is always armed when a
# download or an extraction aborts halfway.
# ---------------------------------------------------------------------------

tmpdir=''
cleanup() {
	[ -n "$tmpdir" ] && [ -d "$tmpdir" ] && rm -rf "$tmpdir"
	return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM
trap 'cleanup; exit 129' HUP

command -v mktemp >/dev/null 2>&1 || die 'mktemp is required and was not found.'
tmpdir="$(mktemp -d)" || die 'could not create a temporary directory.'

# ---------------------------------------------------------------------------
# dependencies
#
# All checked up front. Discovering halfway through that we cannot verify a
# checksum is worse than refusing before a single byte is written.
# ---------------------------------------------------------------------------

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
	fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
	fetch_stdout() { wget -qO - "$1"; }
else
	die 'neither curl nor wget is available; cannot download anything.'
fi

# The checksum is the only thing standing between a hijacked CDN and an
# executable on your PATH. If it cannot be computed, the install stops.
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
	die 'no sha256sum and no shasum found, so the download cannot be verified.
Install one of them (coreutils or perl) and run this again. This script will
not install an unverified binary.'
fi

command -v tar >/dev/null 2>&1 || die 'tar is required and was not found.'

# ---------------------------------------------------------------------------
# platform
# ---------------------------------------------------------------------------

uname_out="$(uname -sm)"
uname_os="${uname_out%% *}"
uname_arch="${uname_out##* }"

case "$uname_os" in
Linux) os='linux' ;;
Darwin) os='darwin' ;;
MINGW* | MSYS* | CYGWIN* | Windows_NT)
	die "this script installs the unix builds only.
On Windows, download vector_<version>_windows_amd64.zip from
https://github.com/${REPO}/releases/latest and put vector.exe on your PATH."
	;;
*)
	die "unsupported operating system: ${uname_os}.
vector publishes builds for linux and darwin (and a windows zip).
If you need ${uname_os}, build from source: go build ./cmd/vector"
	;;
esac

case "$uname_arch" in
x86_64 | amd64) arch='amd64' ;;
aarch64 | arm64) arch='arm64' ;;
*)
	die "unsupported architecture: ${uname_arch}.
vector publishes amd64 and arm64 builds only.
If you need ${uname_arch}, build from source: go build ./cmd/vector"
	;;
esac

# ---------------------------------------------------------------------------
# version
# ---------------------------------------------------------------------------

if [ -n "${VECTOR_VERSION:-}" ]; then
	tag="$VECTOR_VERSION"
else
	api_url="https://api.github.com/repos/${REPO}/releases/latest"
	api_body="$(fetch_stdout "$api_url")" || die "could not reach ${api_url}.
Set VECTOR_VERSION=v1.2.3 to skip the lookup and install a known tag."
	# One field per line, then take the fourth quoted token of the tag_name
	# line. Avoids depending on jq, which most machines do not have.
	tag="$(printf '%s\n' "$api_body" | tr ',' '\n' | grep '"tag_name"' | head -n 1 | cut -d '"' -f 4)" || tag=''
	[ -n "$tag" ] || die "could not read a release tag from the GitHub API.
Set VECTOR_VERSION=v1.2.3 to install a specific tag instead."
fi

# Accept "1.2.3" and "v1.2.3" from the user and normalise both. Asset names
# carry the bare number; the tag in the URL carries the v.
version="${tag#v}"
tag="v${version}"

asset="${BIN}_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/${REPO}/releases/download/${tag}"

# ---------------------------------------------------------------------------
# download and verify
# ---------------------------------------------------------------------------

log "vector ${tag} (${os}/${arch})"

log "downloading ${asset}"
fetch "${base_url}/${asset}" "${tmpdir}/${asset}" ||
	die "could not download ${base_url}/${asset}
Check that ${tag} exists and publishes a ${os}/${arch} build."

log 'downloading checksums.txt'
fetch "${base_url}/checksums.txt" "${tmpdir}/checksums.txt" ||
	die "could not download ${base_url}/checksums.txt
Refusing to install without a checksum to verify against."

# Exact string match on the filename rather than a grep pattern: the asset
# name contains dots, and a regex would happily accept a near-miss line.
# The redirect is on the loop, not on stdin, so this still works under
# `curl | sh`. The `|| [ -n "$sum" ]` tail catches a final line with no
# trailing newline.
expected=''
while read -r sum name || [ -n "$sum" ]; do
	name="${name#\*}" # sha256sum -b marks binary-mode entries with a star
	if [ "$name" = "$asset" ]; then
		expected="$sum"
		break
	fi
done <"${tmpdir}/checksums.txt"

[ -n "$expected" ] || die "checksums.txt does not list ${asset}.
Refusing to install an unverified binary."

actual="$(sha256 "${tmpdir}/${asset}")"
if [ "$actual" != "$expected" ]; then
	die "checksum mismatch for ${asset}
  expected ${expected}
  got      ${actual}
Refusing to install. This means the download was corrupted or tampered with."
fi
log 'checksum ok'

# ---------------------------------------------------------------------------
# install
# ---------------------------------------------------------------------------

tar -xzf "${tmpdir}/${asset}" -C "$tmpdir" || die "could not extract ${asset}."
[ -f "${tmpdir}/${BIN}" ] || die "the archive did not contain a ${BIN} binary."

install_dir="${VECTOR_INSTALL_DIR:-${HOME}/.local/bin}"
mkdir -p "$install_dir" || die "could not create ${install_dir}."
[ -w "$install_dir" ] || die "${install_dir} is not writable.
Set VECTOR_INSTALL_DIR to somewhere you own, for example:
  VECTOR_INSTALL_DIR=\"\$HOME/bin\" sh install.sh"

chmod 0755 "${tmpdir}/${BIN}"
# Move within the filesystem when possible; fall back to a copy when the temp
# directory lives on a different device.
mv -f "${tmpdir}/${BIN}" "${install_dir}/${BIN}" 2>/dev/null ||
	cp -f "${tmpdir}/${BIN}" "${install_dir}/${BIN}" ||
	die "could not write ${install_dir}/${BIN}."

log "installed ${install_dir}/${BIN}"

# ---------------------------------------------------------------------------
# prove it runs
#
# A binary for the wrong architecture installs fine and only fails later, at
# the worst possible moment. Execute it here instead.
# ---------------------------------------------------------------------------

if ! "${install_dir}/${BIN}" --help >/dev/null 2>&1; then
	die "${install_dir}/${BIN} was installed but does not run.
This usually means the wrong build for this machine (${os}/${arch})."
fi

# ---------------------------------------------------------------------------
# PATH
#
# The single most common failure after a successful install: the binary is on
# disk and the shell cannot see it. Say so explicitly, with the exact line.
# ---------------------------------------------------------------------------

case ":${PATH}:" in
*":${install_dir}:"*)
	log ''
	log "vector ${tag} is ready. Try: vector doctor"
	;;
*)
	shell_name="$(basename "${SHELL:-sh}")"
	case "$shell_name" in
	fish)
		rc="${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish"
		line="fish_add_path ${install_dir}"
		;;
	zsh)
		rc="${ZDOTDIR:-$HOME}/.zshrc"
		line="export PATH=\"${install_dir}:\$PATH\""
		;;
	bash)
		# macOS bash reads .bash_profile for login shells; Linux bash reads
		# .bashrc for the interactive ones people actually use.
		if [ "$os" = 'darwin' ]; then
			rc="${HOME}/.bash_profile"
		else
			rc="${HOME}/.bashrc"
		fi
		line="export PATH=\"${install_dir}:\$PATH\""
		;;
	*)
		rc="${HOME}/.profile"
		line="export PATH=\"${install_dir}:\$PATH\""
		;;
	esac

	warn ''
	warn "${install_dir} is not on your PATH, so typing 'vector' will not find it yet."
	warn ''
	warn "Add this line to ${rc}:"
	warn ''
	warn "  ${line}"
	warn ''
	warn 'Then reload your shell, or run that line once in this session.'
	warn "Until then, the full path works: ${install_dir}/${BIN} doctor"
	;;
esac
