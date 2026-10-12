# Repository instructions

## Shared development rules

- Read the project memory map before broad exploration. Keep developer commands and architecture in README.md.
- Direct source dependencies toward business rules. Define small ports at their consumer and assemble implementations explicitly.
- Inject time, IDs and external mechanisms. Use Europe/Moscow unless the contract specifies another zone.
- Keep contracts typed, validate business values, handle errors and preserve wrapped causes.
- Protect shared mutable data and give goroutines an owner, cancellation and completion.
- Keep fixtures and fault injection in tests/examples. Remove replaced code. Comment only non-obvious behavior.
- Preserve unrelated edits. Inspect status and both diffs; stage reviewed paths. Do not hide failures with exclusions or skipped tests.
- Use Conventional Commits. Mark incompatible behavior with ! or BREAKING CHANGE and document migration. No Co-Authored-By or generated-by footers.
- Use SemVer for public contracts. Before 1.0, compatible fixes increment patch; additions/breaking changes increment minor. Published versions/tags are immutable.
- Use annotated vX.Y.Z release tags. Nested published Go modules prefix the module directory; Go v2+ uses matching major module/import paths.
- Release repositories/modules independently. Umir images remain sha-<full-SHA>; factory pins exact component identities.
- Do not rewrite shared history or create/push releases without specific authorization.
- Record actual checks and limitations. Run git diff --check before handoff.

## Repository boundary

Feedback owns intake, notification delivery and future email challenges. Gateway owns accounts, sessions and public guards. Production must not import other service modules or factory code. Delivery adapters depend on consumer-owned application ports. Keep SMTP and Telegram mechanisms outside the core.

## Go verification

Effective policy: .go-quality-versions, .go-quality-modules, .golangci.yml and .go-arch-lint.yml. Quality tools stay outside production dependencies.

- Install exact tools with make tools or use the same versions on PATH.
- Format explicit paths with make fmt FILES="path.go". Reserve fmt-all for separate full formatting.
- Run make check for module inventory, non-mutating format, build, lint, architecture and fresh race tests.
- Run make quality-test after enforcement changes.
- Service checks use GOWORK=off. Unknown packages/modules fail; empty planned components need no placeholders. The shared runner supports only the root module; nested modules require separate effective gates and fail explicitly.
- Handle HTTP encoding, reading, writing and response cleanup errors.
- Run CI quality checks. Container build and publication must be added with the runtime integration task.
