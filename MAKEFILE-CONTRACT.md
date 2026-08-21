# Makefile contract (jim-technologies open-source repositories)

Every repository answers these verbs with these exact names and meanings.
`make help` lists them (the `## comment` convention). The Makefile is a
router: targets delegate to native tooling or guard; logic lives in scripts.

## Required verbs

| Verb | Meaning |
|---|---|
| `make fmt` | Rewrite formatting in place, every language in the repo |
| `make test` | The full test suite, offline and hermetic. Polyglot repos MAY add `test-<lang>` sub-verbs; `test` runs them all |
| `make validate` | The gate, runnable locally, exactly what CI runs: public-surface guard + version parity + format check + lint + typecheck + `test`. CI never checks more or less than this verb |
| `make build` | Produce the artifacts locally |
| `make generate` | (Required where code is generated from schemas) Regenerate; `validate` fails if committed output is stale |
| `make release` | Publish to public ecosystems only — git tag for Go modules, npm, PyPI, crates.io — with one VERSION shared by every language SDK. MUST refuse a dirty or unpushed tree. Runs from a maintainer's machine; CI never publishes |

## The public-surface guard (part of `validate`)

`validate` MUST fail if internal or private codenames, private
infrastructure names, or references to private repositories appear anywhere
in code, docs, examples, or the Makefile itself.

The guard is `scripts/public-surface-check`, one shared implementation across
every public repository. It scans three streams: the content of every tracked
file, every tracked path, and the commit messages a push would publish. It
self-validates before every scan, so it cannot pass by having stopped
checking, and `scripts/public-surface-check-test` proves that from the same
verb. Exceptions are one justified line each in `.public-surface-allow`;
denials this repository adds beyond the fleet baseline live in
`.public-surface-deny`. Neither is made by editing the shared guard.

## Explicitly FORBIDDEN

- `make deploy` — libraries deploy nowhere
- Dependencies that resolve only privately (every `go.mod`/`package.json`/
  lockfile entry must build for a stranger)
- CI secrets of any kind — CI runs `make validate`, nothing else
- Any encrypted-secret store; these repositories hold no secrets

## Required furniture

`LICENSE` · `CHANGELOG.md` · a single `VERSION` file shared by every language
SDK · `make help` self-documentation
