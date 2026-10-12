#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$root/.go-quality-versions"
export GO_QUALITY_BIN="${GO_QUALITY_BIN:-$root/.cache/go-quality/bin}"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/go-quality-fixtures.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
fixture="$scratch/project"
mkdir -p "$fixture/scripts" "$fixture/cmd/server"
cp "$root/.golangci.yml" "$root/.go-arch-lint.yml" "$root/.go-quality-versions" "$root/.go-quality-modules" "$root/go.mod" "$fixture/"
cp "$root/scripts/go-quality.sh" "$fixture/scripts/"
module="$(awk '$1 == "module" {print $2}' "$fixture/go.mod")"
printf 'package main\n\nfunc main() {}\n' > "$fixture/cmd/server/main.go"
passed=0
run() { bash "$fixture/scripts/go-quality.sh" "$@"; }
reject() {
  local action="$1" expected="$2"
  if run "$action" > "$scratch/output" 2>&1; then
    printf 'FAIL: %s accepted %s\n' "$action" "$expected" >&2
    exit 1
  fi
  if ! grep -F "$expected" "$scratch/output" >/dev/null; then
    cat "$scratch/output" >&2
    printf 'FAIL: unexpected rejection, wanted %s\n' "$expected" >&2
    exit 1
  fi
  passed=$((passed + 1))
}
run check > "$scratch/positive" 2>&1 || { cat "$scratch/positive"; exit 1; }
[[ ! -e "$fixture/server" ]] || { printf 'Build output leaked into the source checkout\n' >&2; exit 1; }
passed=$((passed + 1))
mv "$fixture/cmd/server/main.go" "$scratch/main.go"
if grep -q '^  domain:' "$root/.go-arch-lint.yml"; then library_dir=internal/domain; else library_dir=internal/proxy; fi
mkdir -p "$fixture/$library_dir"
printf 'package %s\n\ntype ID string\n' "${library_dir##*/}" > "$fixture/$library_dir/library.go"
run check > "$scratch/library-positive" 2>&1 || { cat "$scratch/library-positive"; exit 1; }
passed=$((passed + 1))
rm "$fixture/$library_dir/library.go"
mv "$scratch/main.go" "$fixture/cmd/server/main.go"
mkdir -p "$fixture/tests/integration/nested"
printf 'module example.com/nested\n\ngo 1.26.5\n' > "$fixture/tests/integration/nested/go.mod"
printf 'package nested\n\nvar Broken = missing\n' > "$fixture/tests/integration/nested/broken.go"
cp "$fixture/.go-quality-modules" "$scratch/modules"
printf '\ntests/integration/nested\n' >> "$fixture/.go-quality-modules"
for action in check build lint arch test fmt-check; do
  reject "$action" 'Nested modules require separate quality gates'
done
mv "$scratch/modules" "$fixture/.go-quality-modules"
rm "$fixture/tests/integration/nested/go.mod" "$fixture/tests/integration/nested/broken.go"
printf 'package main\nfunc main( ){ }\n' > "$fixture/cmd/server/main.go"
reject fmt-check 'Formatting differences'
printf 'package main\n\nfunc main() {}\n' > "$fixture/cmd/server/main.go"
mkdir -p "$fixture/internal/unclassified"
printf 'package unclassified\n\nconst Value = 1\n' > "$fixture/internal/unclassified/new.go"
reject arch 'not attached'
rm "$fixture/internal/unclassified/new.go"
printf 'package unclassified\n\nimport "testing"\n\nfunc TestExample(t *testing.T) {}\n' > "$fixture/internal/unclassified/new_test.go"
reject arch 'not attached'
rm "$fixture/internal/unclassified/new_test.go"
mkdir -p "$fixture/unlisted-module"
printf 'module example.com/unlisted\n\ngo 1.26.5\n' > "$fixture/unlisted-module/go.mod"
reject build 'Missing or undeclared Go module'
rm "$fixture/unlisted-module/go.mod"
printf 'package main\n\nimport "strconv"\n\nfunc main() { _, _ = strconv.Atoi("invalid") }\n' > "$fixture/cmd/server/main.go"
gofmt -w "$fixture/cmd/server/main.go"
reject lint 'errcheck'
printf 'package main\n\nimport "strconv"\n\nfunc main() {\n//nolint:errcheck\n_, _ = strconv.Atoi("invalid")\n}\n' > "$fixture/cmd/server/main.go"
gofmt -w "$fixture/cmd/server/main.go"
reject lint 'nolintlint'
printf 'package main\n\nimport "strconv"\n\nfunc main() {\n//nolint\n_, _ = strconv.Atoi("invalid")\n}\n' > "$fixture/cmd/server/main.go"
gofmt -w "$fixture/cmd/server/main.go"
reject lint 'nolintlint'
printf 'package main\n\nfunc main() { var value any = "ok"; result := value.(string); _ = result }\n' > "$fixture/cmd/server/main.go"
gofmt -w "$fixture/cmd/server/main.go"
reject lint 'errcheck'
printf 'package main\n\nfunc main() {}\n' > "$fixture/cmd/server/main.go"
mkdir -p "$fixture/tests/testsupport"
printf 'package testsupport\n\nconst Value = 1\n' > "$fixture/tests/testsupport/support.go"
printf 'package main\n\nimport "%s/tests/testsupport"\n\nfunc main() { _ = testsupport.Value }\n' "$module" > "$fixture/cmd/server/main.go"
reject lint 'depguard'
reject arch 'testsupport'
printf 'package main\n\nfunc main() {}\n' > "$fixture/cmd/server/main.go"
factory="$scratch/factory"
mkdir -p "$factory"
printf 'module github.com/JustIkra/umir-factory\n\ngo 1.26.5\n' > "$factory/go.mod"
printf 'package umirfactory\n\nconst Name = "factory"\n' > "$factory/factory.go"
go mod edit -require=github.com/JustIkra/umir-factory@v0.0.0 -replace="github.com/JustIkra/umir-factory=$factory" "$fixture/go.mod"
printf 'package main\n\nimport umirfactory "github.com/JustIkra/umir-factory"\n\nfunc main() { _ = umirfactory.Name }\n' > "$fixture/cmd/server/main.go"
reject lint 'github.com/JustIkra/umir-factory'
printf 'package main\n\nfunc main() {}\n' > "$fixture/cmd/server/main.go"
if grep -q '^  domain:' "$root/.go-arch-lint.yml"; then
  mkdir -p "$fixture/internal/domain" "$fixture/internal/application" "$fixture/internal/adapters/memory" "$fixture/bootstrap"
  printf 'package domain\n\ntype ID string\n' > "$fixture/internal/domain/id.go"
  printf 'package application\n\nimport "%s/internal/domain"\n\ntype Port interface { Save(domain.ID) error }\n' "$module" > "$fixture/internal/application/port.go"
  printf 'package memory\n\nimport "%s/internal/domain"\n\ntype Store struct{}\n\nfunc (*Store) Save(domain.ID) error { return nil }\n' "$module" > "$fixture/internal/adapters/memory/store.go"
  printf 'package bootstrap\n\nimport ("%s/internal/application"; "%s/internal/adapters/memory")\n\nvar _ application.Port = (*memory.Store)(nil)\n\nfunc Build() *memory.Store { return &memory.Store{} }\n' "$module" "$module" > "$fixture/bootstrap/bootstrap.go"
  printf 'package main\n\nimport "%s/bootstrap"\n\nfunc main() { _ = bootstrap.Build() }\n' "$module" > "$fixture/cmd/server/main.go"
  run fmt-all > "$scratch/format" 2>&1
  run check > "$scratch/core-positive" 2>&1 || { cat "$scratch/core-positive"; exit 1; }
  passed=$((passed + 1))
  printf 'package domain\n\nimport "net/http"\n\nconst Method = http.MethodGet\n' > "$fixture/internal/domain/invalid.go"
  reject lint 'depguard'
  rm "$fixture/internal/domain/invalid.go"
  mkdir -p "$fixture/contract"
  printf 'package contract\n\nimport "net/http"\n\nconst Method = http.MethodGet\n' > "$fixture/contract/invalid.go"
  reject lint 'depguard'
  rm "$fixture/contract/invalid.go"
  printf 'package domain\n\nimport clock "time"\n\nfunc Now() clock.Time { return clock.Now() }\n' > "$fixture/internal/domain/invalid.go"
  reject lint 'forbidigo'
  rm "$fixture/internal/domain/invalid.go"
  for package in domain application; do
    for clock_import in time clock; do
      for clock_call in Since Until; do
        printf 'package %s\n\nimport %s "time"\n\nfunc Invalid(value %s.Time) %s.Duration { return %s.%s(value) }\n' "$package" "$clock_import" "$clock_import" "$clock_import" "$clock_import" "$clock_call" > "$fixture/internal/$package/invalid.go"
        reject lint 'forbidigo'
        rm "$fixture/internal/$package/invalid.go"
      done
    done
  done
  printf 'package domain\n\nimport "time"\n\nfunc Elapsed(start, end time.Time) time.Duration { return end.Sub(start) }\n' > "$fixture/internal/domain/elapsed.go"
  run lint > "$scratch/time-positive" 2>&1 || { cat "$scratch/time-positive"; exit 1; }
  passed=$((passed + 1))
  rm "$fixture/internal/domain/elapsed.go"
  printf 'package application\n\nimport "%s/internal/adapters/memory"\n\nfunc Invalid() *memory.Store { return &memory.Store{} }\n' "$module" > "$fixture/internal/application/invalid.go"
  reject arch 'memory'
  reject lint 'depguard'
  rm "$fixture/internal/application/invalid.go"
fi
mv "$fixture/go.mod" "$scratch/go.mod"
reject build 'Missing or undeclared Go module'
mv "$scratch/go.mod" "$fixture/go.mod"
mkdir -p "$scratch/fake-tools"
original_bin="${GO_QUALITY_BIN-}"
export GO_QUALITY_BIN="$scratch/fake-tools"
printf '#!/usr/bin/env bash\nprintf "golangci-lint has version 0.0.0 built for a fixture\\n"\n' > "$GO_QUALITY_BIN/golangci-lint"
chmod +x "$GO_QUALITY_BIN/golangci-lint"
reject fmt-check "Expected golangci-lint $GOLANGCI_LINT_VERSION"
printf '#!/usr/bin/env bash\nif [[ "$1" == version ]]; then printf "golangci-lint has version %s built for a fixture\\n"; else printf "formatter fixture failure\\n" >&2; exit 7; fi\n' "$GOLANGCI_LINT_VERSION" > "$GO_QUALITY_BIN/golangci-lint"
set +e
run fmt-check > "$scratch/tool-failure" 2>&1
status=$?
set -e
[[ "$status" == 7 ]] || { cat "$scratch/tool-failure"; printf 'Formatter exit status was lost: %s\n' "$status" >&2; exit 1; }
passed=$((passed + 1))
if [[ -n "$original_bin" ]]; then export GO_QUALITY_BIN="$original_bin"; else unset GO_QUALITY_BIN; fi
printf 'Quality enforcement: %d scenarios passed\n' "$passed"
