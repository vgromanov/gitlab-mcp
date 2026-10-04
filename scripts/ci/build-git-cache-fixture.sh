#!/bin/sh
# Build the pinned Git 2.50.1 fixture into RUNNER_TEMP. No install, no sudo.
set -eu

if [ -z "${RUNNER_TEMP:-}" ]; then
	echo "RUNNER_TEMP is required" >&2
	exit 1
fi

url="https://www.kernel.org/pub/software/scm/git/git-2.50.1.tar.xz"
sum="7e3e6c36decbd8f1eedd14d42db6674be03671c2204864befa2a41756c5c8fc4"
arc="$RUNNER_TEMP/git-2.50.1.tar.xz"

curl -fsSL "$url" -o "$arc"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$arc" | awk '{print $1}')
else
	got=$(shasum -a 256 "$arc" | awk '{print $1}')
fi
if [ "$got" != "$sum" ]; then
	echo "git archive hash mismatch" >&2
	exit 1
fi

rm -rf "$RUNNER_TEMP/git-src"
mkdir -p "$RUNNER_TEMP/git-src"
tar -xJf "$arc" -C "$RUNNER_TEMP/git-src"
cd "$RUNNER_TEMP/git-src/git-2.50.1"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
flag_csv="NO_CURL=YesPlease,NO_EXPAT=YesPlease,NO_GETTEXT=YesPlease,NO_TCLTK=YesPlease,NO_OPENSSL=YesPlease,NO_APPLE_COMMON_CRYPTO=YesPlease"
set -- \
	NO_CURL=YesPlease \
	NO_EXPAT=YesPlease \
	NO_GETTEXT=YesPlease \
	NO_TCLTK=YesPlease \
	NO_OPENSSL=YesPlease \
	NO_APPLE_COMMON_CRYPTO=YesPlease
case "$os" in
linux)
	set -- "$@" NO_ICONV=YesPlease
	flag_csv="${flag_csv},NO_ICONV=YesPlease"
	;;
darwin) ;;
*) echo "unsupported fixture os" >&2; exit 1 ;;
esac
make -j2 "$@" git

test -x git
if command -v sha256sum >/dev/null 2>&1; then
	bin=$(sha256sum git | awk '{print $1}')
else
	bin=$(shasum -a 256 git | awk '{print $1}')
fi

machine=$(uname -m)
case "$machine" in
aarch64|arm64) arch=arm64 ;;
x86_64) arch=amd64 ;;
*) arch=$machine ;;
esac
kernel=$(uname -r)
major=0
case "$os" in
darwin) major=$(printf '%s' "$kernel" | cut -d. -f1) ;;
esac
cc=${CC:-cc}
compiler=$("$cc" --version 2>/dev/null | head -n 1 || true)
version=$(./git version)

ident="$RUNNER_TEMP/gitcache-identity"
{
	printf 'source_sha256=%s\n' "$sum"
	printf 'flags=%s\n' "$flag_csv"
	printf 'os=%s\n' "$os"
	printf 'arch=%s\n' "$arch"
	printf 'compiler=%s\n' "$compiler"
	printf 'binary_sha256=%s\n' "$bin"
	printf 'version_line=%s\n' "$version"
	printf 'kernel=%s\n' "$kernel"
	printf 'darwin_major=%s\n' "$major"
	printf 'git=%s\n' "$RUNNER_TEMP/git-src/git-2.50.1/git"
} > "$ident"
