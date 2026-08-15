#!/bin/sh
# Fleetplane installer — downloads the latest release binary for this
# platform, verifies its checksum, and installs it.
#
#   curl -fsSL https://raw.githubusercontent.com/samishal1998/fleetplane/main/install.sh | sh
#
# Environment overrides:
#   FLEETPLANE_VERSION      pin a release tag (default: latest), e.g. v0.4.0
#   FLEETPLANE_INSTALL_DIR  install directory (default: /usr/local/bin when
#                           writable, else ~/.local/bin)
set -eu

REPO="samishal1998/fleetplane"
VERSION="${FLEETPLANE_VERSION:-}"
INSTALL_DIR="${FLEETPLANE_INSTALL_DIR:-}"

say() { printf '%s\n' "$*" >&2; }
fail() { say "fleetplane install: error: $*"; exit 1; }

command -v curl >/dev/null 2>&1 || fail "curl is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

os=$(uname -s)
case "$os" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported OS '$os' — prebuilt binaries cover Linux and macOS; elsewhere use: go install github.com/$REPO/cmd/fleetplane@latest" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported architecture '$arch' (amd64 and arm64 are published)" ;;
esac

if [ -z "$VERSION" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name"[^"]*"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$VERSION" ] || fail "could not resolve the latest release (rate limit or no releases yet); pin one with FLEETPLANE_VERSION=vX.Y.Z"
fi

asset="fleetplane_${VERSION}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "fleetplane: downloading $VERSION ($os/$arch)..."
curl -fsSL -o "$tmp/$asset" "$base/$asset" || fail "download failed: $base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "checksum manifest download failed"

expected=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] || fail "no checksum recorded for $asset"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
else
  fail "neither sha256sum nor shasum found to verify the download"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $asset (got $actual, want $expected)"

tar -xzf "$tmp/$asset" -C "$tmp" fleetplane
[ -f "$tmp/fleetplane" ] || fail "archive did not contain the fleetplane binary"

if [ -z "$INSTALL_DIR" ]; then
  if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
    INSTALL_DIR=/usr/local/bin
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR" || fail "cannot create $INSTALL_DIR (set FLEETPLANE_INSTALL_DIR to a writable directory)"
if ! install -m 0755 "$tmp/fleetplane" "$INSTALL_DIR/fleetplane" 2>/dev/null; then
  cp "$tmp/fleetplane" "$INSTALL_DIR/fleetplane"
  chmod 0755 "$INSTALL_DIR/fleetplane"
fi

say "fleetplane: installed $("$INSTALL_DIR/fleetplane" version 2>/dev/null || echo "$VERSION") -> $INSTALL_DIR/fleetplane"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "fleetplane: note: $INSTALL_DIR is not on your PATH — add it, e.g.: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
say "fleetplane: next steps: https://github.com/$REPO/blob/main/SETUP_GUIDE.md"
