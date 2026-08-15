#!/usr/bin/env bash
# Transitive dependency boundary checks (ADR-009).
#
# golangci-lint's depguard sees only direct imports; this script walks the full
# transitive closure with `go list -deps`. All rules fail closed. Run with
# --self-test to prove every rule fires on planted violations.
set -euo pipefail

M=github.com/samishal1998/fleetplane
FORBIDDEN_IN_KERNEL="$M/providers|hetznercloud|aws-sdk-go|google.golang.org/api|go-github"

fail=0

check() { # name, deps (newline-separated), forbidden-pattern
  local name="$1" deps="$2" pattern="$3" hits
  hits=$(grep -E "$pattern" <<<"$deps" || true)
  if [[ -n "$hits" ]]; then
    { echo "BOUNDARY VIOLATION ($name):"; echo "$hits"; } >&2
    fail=1
  fi
}

check_sdk_stdlib_only() { # deps
  # Allowed: pkg/sdk itself, and the standard library (bare first path segment
  # without a dot, plus internal/ and vendor/ std shims).
  local deps="$1" hits
  hits=$(grep -vE "^($M/pkg/sdk|internal/|vendor/|[a-z0-9]+(/|$))" <<<"$deps" || true)
  if [[ -n "$hits" ]]; then
    { echo "BOUNDARY VIOLATION (pkg/sdk must be stdlib-only):"; echo "$hits"; } >&2
    fail=1
  fi
}

if [[ "${1:-}" == "--self-test" ]]; then
  check "self-test: kernel purity" "$M/providers/hetzner" "$FORBIDDEN_IN_KERNEL"
  [[ $fail -eq 1 ]] || { echo "self-test FAILED: kernel-purity rule did not fire" >&2; exit 1; }
  fail=0
  check "self-test: provider isolation" "$M/internal/storage" "$M/internal"
  [[ $fail -eq 1 ]] || { echo "self-test FAILED: provider-isolation rule did not fire" >&2; exit 1; }
  fail=0
  check_sdk_stdlib_only "github.com/spf13/cobra"
  [[ $fail -eq 1 ]] || { echo "self-test FAILED: sdk-stdlib-only rule did not fire" >&2; exit 1; }
  echo "check-boundaries self-test OK (all rules fire)"
  exit 0
fi

list_deps() { go list -deps "$@" 2>/dev/null || true; }

check "kernel/pkg must not depend on providers or cloud SDKs" \
  "$(list_deps ./internal/... ./pkg/...)" "$FORBIDDEN_IN_KERNEL"

check "providers must not depend on internal" \
  "$(list_deps ./providers/...)" "$M/internal"

sdk_deps=$(list_deps ./pkg/sdk/...)
[[ -z "$sdk_deps" ]] || check_sdk_stdlib_only "$sdk_deps"

[[ $fail -eq 0 ]] || exit 1
echo "boundaries OK"
