---
description: |
  Write commit messages and PR titles in scoped-commit format ("scope: description"), not Conventional Commits. Use when writing commit messages, creating commits, titling pull requests, squash-merging, or when the user asks about commit message style. Scope is mandatory and comes first; type prefixes (feat/fix/chore) are dropped.
name: scoped-commit
---

# Scoped Commits

Scoped commits, not Conventional Commits. The scope is the most useful piece of
information in a commit message, so it goes first and it is mandatory. The type
(`feat`, `fix`, `chore`) is dropped: a good description already implies the type,
and the type prefix wastes subject-line space on metadata nobody scans for.

Inspired by the commit conventions of Linux, Git, Go, and nixpkgs.

## When this skill applies

Always default to scoped commits. The only exception: the repo's `AGENTS.md`
or `CONTRIBUTING` files explicitly define a different commit style. In that
case, follow the repo docs. Do not infer a convention from the git log —
existing history is often inconsistent, and a popular style in the log is not
an explicit project decision.

## Format

```
scope: description

[optional body]

[optional footer(s)]
```

- **Scope is required.** A commit without a scope is a sentence without a subject.
- **Description** is imperative, starts lowercase, no trailing period. It says
  what the change does, not what kind of change it is.
- **Subject line ≤ 72 characters.** Move detail to the body.
- **Body** (optional) explains _why_, links context, notes trade-offs. Wrap at 80.
- **PR number** is appended by squash-merge (`... (#123)`). Do not type it by hand.
- **No ticket IDs in the scope.** If a ticket reference is mandatory, put it in
  the body or a footer (`Refs: PROJ-1234`), never where the scope belongs.

## PR titles are commit messages

When the repo squash-merges via the GitHub UI, **the PR title becomes the commit
subject line** on the default branch (GitHub appends the PR number). Therefore:

- PR titles follow the same `scope: description` format as commit messages.
- Write the PR title as the commit you want to land, not as a branch summary.
  `api: add request timeout middleware`, not `Timeout feature branch`.
- The PR body maps to the commit body: put the _why_ there. Trim GitHub
  template boilerplate before merging so it does not land in the commit log.
- In-branch commits can be looser (`wip`, `bump`) since squash-merge erases
  them — but keeping them scoped makes review easier and protects you if the
  PR ever merges without squashing.

## Choosing the scope

Use the **narrowest scope that honestly contains the change**:

1. **Subsystem** — the default. Top-level component of the repo.
2. **Package or area** — when the change is narrowly focused on one package,
   one provider, or one feature area, use that directly (optionally as
   `subsystem/area` for precision).

Derive scopes from the directory layout. When in doubt, the scope is the
directory a reviewer would `cd` into. Check the repo for a documented scope
catalog (for example a `COMMITS.md`) before inventing new scopes.

### Example catalog (generic service repository)

| Scope    | Covers                                                       |
| -------- | ------------------------------------------------------------ |
| `api`    | HTTP API, request handlers, validation, and public contracts |
| `worker` | Background jobs, queues, retries, and scheduling             |
| `web`    | Browser UI, routes, components, and client state             |
| `db`     | Schema, migrations, queries, and storage adapters            |
| `auth`   | Identity, sessions, permissions, and access control          |
| `deploy` | Charts, deployment config, and environment wiring            |
| `docs`   | Documentation-only changes                                   |
| `deps`   | Dependency updates with no product behavior change           |

### Cross-cutting changes

- **Split the commit.** One logical change per commit. If a change touches two
  subsystems, it is usually two commits.
- If splitting is genuinely impossible (a rename across the repo, a schema
  change with its migration), use the **broadest common scope** and enumerate
  the affected areas in the body.

## What not to do

- `bump`, `bump chart`, `dummy change` — say _what_ bumped and _why_.
- `fixing merge conflicts` — do not commit conflict-resolution noise; rebase
  instead, or if the resolution was substantive, describe the actual change.
- `cleanup` — cleanup of _what_? `api: drop unused request fixtures` tells a
  story; `cleanup` does not.
- `addressing comment` — the comment is not visible in the log; the change is.
- Type-prefixed multi-change subjects like `feat: add search + switch storage`
  — split it and scope it.

## Rewrites from typical history

| Before                                  | After                                                                   |
| --------------------------------------- | ----------------------------------------------------------------------- |
| `feat(api): add request timeouts (#90)` | `api: add request timeouts (#90)`                                       |
| `fix(worker): retry failed jobs (#72)`  | `worker: retry failed jobs (#72)`                                       |
| `perf(db): reduce query count (#60)`    | `db: reduce query count (#60)`                                          |
| `bumping up secrets (#80)`              | `deploy: bump secrets to v5 (#80)`                                      |
| `Mitigate fire-and-forget risks (#87)`  | `worker: make callbacks at-least-once instead of fire-and-forget (#87)` |
| `UI improvements (#76)`                 | `web: fix empty state on the search page (#76)`                         |
| `feat: add CSV export (#92)`            | `api: add CSV export for reports (#92)`                                 |
| `scheduling fix for worker api (#84)`   | `worker: fix scheduled retry ordering (#84)`                            |
