# Makefile contract (jim-technologies open-source repositories)

This file is byte-identical in every public repository in this organisation.
It states the contract those repositories share, and only that: the verbs, and
the rules behind them, never the tools that implement them. Which formatter,
which test runner, which package ecosystem a repository uses is that
repository's own business and belongs in its own README or AGENTS.md. Nothing
here changes with the language a repository is written in.

## The Makefile is a router, not a program

`make` is the entry point, so that every task in every repository has one name
and one standard way to run it. A new way of doing something replaces the old
way instead of joining it: two verbs for the same work are a defect, not a
convenience.

A target names the tool or the script that does the work, and stops there.
Loops, conditionals, staleness comparisons and multi-step pipelines move into
`scripts/`, where they can be read, run and tested on their own. A target that
has grown past a few lines is a script that has not been written yet. Logic
inside a Makefile is logic nobody can run without `make`, and nobody can test
at all.

## The gate

`make validate` is the gate: one verb, runnable by anyone with a clone and the
repository's toolchain, that decides whether the tree is fit to push. It is
also the ceiling. Where a repository has CI, CI runs the gate and nothing the
gate does not run; CI that fans the work out — per package, per language, per
platform — is scheduling the same checks, not adding to them. A green
`make validate` is the whole answer.

Some repositories reached the gate under an earlier name and keep that name
working: `make ci`, or `make lint` and `make test` as its two halves. Those
still run, and `make validate` runs exactly what they run. `validate` is the
name this contract uses, and the name every one of these repositories answers
to.

## The verbs

| Verb | Where | Meaning |
|---|---|---|
| `make validate` | always | The gate, above: the public-surface guard and its self-test, format and lint checks, type checks, generated-output staleness, and `test`. A repository adds its own checks to this list; it never subtracts. |
| `make test` | always | The full suite for everything the repository ships. What the gate runs is offline and hermetic — no network, no credential, no service started by hand; anything needing those is a separate opt-in tier the gate never runs. Polyglot repositories may add `test-<lang>` sub-verbs, and `test` runs them all. |
| `make fmt` | where the repository formats code | Rewrite formatting in place, every language in the repository. `validate` checks formatting and never rewrites it. |
| `make build` | where the repository produces artifacts | Produce them locally, from the tree as committed. |
| `make generate` | where code is generated from a schema | Regenerate the committed output. `validate` fails when what is committed differs from what `generate` produces. |
| `make release` | where the repository publishes packages | Publish to public ecosystems only, from a maintainer's machine, refusing a dirty or unpushed tree. CI never publishes. |
| `make help` | recommended | List the verbs, from the `## ` comments on the targets. |

A repository may add verbs. It may not give these words a second meaning, and
it may not reach one of these jobs under a different word.

## The public-surface guard

`validate` runs `scripts/public-surface-check`, and runs
`scripts/public-surface-check-test` beside it, so the gate goes red if the
guard itself stops working.

The guard scans three streams: the content of every tracked file, every
tracked path, and the commit messages a push would publish. A finding against
a commit message means the message must be rewritten before the branch leaves
the machine — fixing the file is not enough. Before each scan the guard
re-runs every category against its own probes, so it cannot pass by having
quietly stopped checking.

It denies private repository and host names, internal infrastructure and
datastore names, product codenames, cluster and address shapes, credential
shapes, encrypted secret stores and key material, and private git remotes and
package registries.

Exceptions are never made by editing the guard. Each repository keeps its own
two files at its root, both in the same four fields —
`category | path-glob | reason | pattern`:

- `.public-surface-allow`, the justified exceptions. The reason is required
  and is what the next reviewer reads. A rule masks exactly the text its
  pattern names, only under the paths it names, only for its category;
  everything else on the line is still checked, and a rule broad enough to
  switch a whole category off is rejected rather than obeyed.
- `.public-surface-deny`, optional, for denials a repository adds on top of
  the fleet baseline. Its reason is printed as the remediation when the rule
  fires, so it is written as an instruction to whoever tripped it.

The guard and its self-test are a fleet artifact: one implementation, one
filename, one invocation, identical byte for byte in every public repository.
Changing either is a change to all of them at once — it lands everywhere in
the same campaign, or it does not land. One repository's copy drifting is a
defect, and the two files above are the reason no repository ever needs to.

## No secrets, and nothing that needs one

- No repository tracks a credential: no key material, no encrypted store or
  configuration for one, no environment file carrying values. Documenting how
  an operator supplies a credential at runtime is part of what some of these
  repositories publish; shipping any part of the credential itself is not.
- No workflow reads a repository, environment or organisation secret. The gate
  is the whole of CI, and the gate needs nothing a stranger cannot supply, so
  a check that would need a credential is not part of the gate. Where a
  repository runs network-dependent audits, they run outside the gate and hold
  no secret either.
- No dependency resolves only privately. Every lockfile entry, module path and
  registry a repository names has to resolve for a stranger over public HTTPS.
- No verb deploys. The Makefile builds, checks and publishes; it never
  operates a running system. Where a repository is an application that ships
  deployment tooling, that tooling is documentation and configuration an
  operator applies, not a target the Makefile fires.

## Commit messages

One line. A commit message is a single subject line: no body, no trailers, no
co-author or generated-by lines, no attribution of any kind. Each repository
keeps its own subject idiom — a scope prefix, a leading capital, an imperative
verb — and stays consistent with its own log. Reasoning that needs a paragraph
belongs in the documentation, where the next reader will look for it.

The guard reads the messages a push would publish, so a subject line is public
surface too. A private name in a subject fails the gate exactly as it would in
a file, and the fix is to rewrite the message before pushing.

## Required furniture

`LICENSE`; a `README.md` a stranger can act on; `scripts/public-surface-check`
and `scripts/public-surface-check-test` with `.public-surface-allow` beside
them; and a Makefile whose verbs mean what this file says. Where a repository
publishes versioned packages it also keeps `CHANGELOG.md` and a single
`VERSION` that every language package is generated from or checked against.
