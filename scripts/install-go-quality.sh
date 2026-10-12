#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$root/.go-quality-versions"
destination="${GO_QUALITY_BIN:-$root/.cache/go-quality/bin}"
case "$(uname -s)" in
  Darwin) platform=darwin ;;
  Linux) platform=linux ;;
  *) printf 'Unsupported quality tool platform\n' >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) printf 'Unsupported quality tool architecture\n' >&2; exit 1 ;;
esac
scratch="$(mktemp -d "${TMPDIR:-/tmp}/go-quality-install.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$destination"
download() {
  curl --fail --silent --show-error --location --retry 2 "$1" --output "$scratch/$2"
}
verify() {
  local archive="$1" checksums="$2" expected actual
  expected="$(awk -v file="$archive" '$2 == file || $2 == "*"file { print $1 }' "$scratch/$checksums")"
  [[ "$expected" =~ ^[[:xdigit:]]{64}$ ]] || { printf 'Missing checksum: %s\n' "$archive" >&2; exit 1; }
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$scratch/$archive" | awk '{print $1}')"
  else
    actual="$(shasum -a 256 "$scratch/$archive" | awk '{print $1}')"
  fi
  [[ "$actual" == "$expected" ]] || { printf 'Checksum mismatch: %s\n' "$archive" >&2; exit 1; }
}
lint_archive="golangci-lint-$GOLANGCI_LINT_VERSION-$platform-$arch.tar.gz"
lint_base="https://github.com/golangci/golangci-lint/releases/download/v$GOLANGCI_LINT_VERSION"
download "$lint_base/$lint_archive" "$lint_archive"
download "$lint_base/golangci-lint-$GOLANGCI_LINT_VERSION-checksums.txt" lint-checksums
verify "$lint_archive" lint-checksums
mkdir "$scratch/lint"
tar -xzf "$scratch/$lint_archive" -C "$scratch/lint"
install -m 0755 "$scratch/lint/golangci-lint-$GOLANGCI_LINT_VERSION-$platform-$arch/golangci-lint" "$destination/golangci-lint"
arch_archive="go-arch-lint_${GO_ARCH_LINT_VERSION}_${platform}_${arch}.tar.gz"
arch_base="https://github.com/fe3dback/go-arch-lint/releases/download/v$GO_ARCH_LINT_VERSION"
download "$arch_base/$arch_archive" "$arch_archive"
download "$arch_base/checksums.txt" arch-checksums
verify "$arch_archive" arch-checksums
mkdir "$scratch/arch"
tar -xzf "$scratch/$arch_archive" -C "$scratch/arch"
install -m 0755 "$scratch/arch/go-arch-lint" "$destination/go-arch-lint"
printf 'Installed pinned quality tools in %s\n' "$destination"
