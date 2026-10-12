#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
source .go-quality-versions
export GOWORK=off
scratch="$(mktemp -d "${TMPDIR:-/tmp}/go-quality-check.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
action="${1:-check}"
shift "$(( $# > 0 ? 1 : 0 ))"
inventory() {
  find . -type d \( -name .git -o -name .cache -o -name vendor -o -name node_modules -o -name testdata \) -prune -o -type f -name go.mod -print | sed 's#/go.mod$##;s#^\./##' | LC_ALL=C sort > "$scratch/actual-modules"
  sed '/^[[:space:]]*#/d;/^[[:space:]]*$/d' .go-quality-modules | LC_ALL=C sort > "$scratch/expected-modules"
  if ! diff -u "$scratch/expected-modules" "$scratch/actual-modules"; then
    printf 'Missing or undeclared Go module; update the reviewed inventory\n' >&2
    exit 1
  fi
  if [[ "$(cat "$scratch/expected-modules")" != "." ]]; then
    printf 'Nested modules require separate quality gates; this runner checks only the root module\n' >&2
    exit 1
  fi
}
tool() {
  local name="$1" version="$2" candidate output
  candidate="${GO_QUALITY_BIN:-$root/.cache/go-quality/bin}/$name"
  if [[ ! -x "$candidate" ]]; then candidate="$(command -v "$name" || true)"; fi
  [[ -n "$candidate" ]] || { printf 'Missing %s; run make tools\n' "$name" >&2; return 1; }
  if [[ "$name" == golangci-lint ]]; then
    output="$("$candidate" version)"
    [[ "$output" == *"version $version "* ]] || { printf 'Expected %s %s: %s\n' "$name" "$version" "$output" >&2; return 1; }
  else
    output="$("$candidate" --output-color=false version)"
    [[ "$output" == *"Linter version: $version"$'\n'* ]] || { printf 'Expected %s %s: %s\n' "$name" "$version" "$output" >&2; return 1; }
  fi
  printf '%s\n' "$candidate"
}
format_check() {
  local lint status=0
  lint="$(tool golangci-lint "$GOLANGCI_LINT_VERSION")"
  "$lint" fmt --config "$root/.golangci.yml" --diff > "$scratch/format.diff" || status=$?
  if [[ -s "$scratch/format.diff" ]]; then
    cat "$scratch/format.diff"
    printf 'Formatting differences found; format explicit changed files\n' >&2
    return 1
  fi
  return "$status"
}
build() {
  local package_names
  package_names="$(go list -f '{{.Name}}' ./...)"
  if grep -qx main <<< "$package_names"; then
    mkdir -p "$scratch/build"
    go build -o "$scratch/build/" ./...
  else
    go build ./...
  fi
}
lint() {
  local lint
  lint="$(tool golangci-lint "$GOLANGCI_LINT_VERSION")"
  "$lint" config verify --config "$root/.golangci.yml"
  "$lint" run --allow-serial-runners --config "$root/.golangci.yml" ./...
}
architecture() {
  local arch
  arch="$(tool go-arch-lint "$GO_ARCH_LINT_VERSION")"
  "$arch" --output-color=false check --project-path "$root" --arch-file "$root/.go-arch-lint.yml"
}
tests() { go test -race -count=1 ./...; }
inventory
case "$action" in
  fmt)
    [[ $# -gt 0 ]] || { printf 'Provide explicit paths: make fmt FILES="path.go"\n' >&2; exit 1; }
    lint="$(tool golangci-lint "$GOLANGCI_LINT_VERSION")"
    "$lint" fmt --config "$root/.golangci.yml" "$@"
    ;;
  fmt-all)
    lint="$(tool golangci-lint "$GOLANGCI_LINT_VERSION")"
    "$lint" fmt --config "$root/.golangci.yml"
    ;;
  fmt-check) format_check ;;
  build) build ;;
  lint) lint ;;
  arch) architecture ;;
  test) tests ;;
  check) format_check; build; lint; architecture; tests ;;
  *) printf 'Unknown quality command: %s\n' "$action" >&2; exit 1 ;;
esac
