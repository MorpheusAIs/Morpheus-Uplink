# Contributing

## Branches

`main` is the only long-lived branch. There is no `dev` or `test` branch.

A merge to `main` that touches a material path (code, `deploy/`, the Dockerfile, the build workflow) publishes a release. Docs-only changes do not. The list of material paths is in [`docs/06-developers.md`](docs/06-developers.md#cicd-and-versioning).

## How to submit a change

1. Branch from current `main`:
   ```bash
   git fetch origin
   git checkout -b fix/your-topic origin/main
   ```
2. Keep the change focused.
3. Open a pull request with base `main`.
4. If review sits for a while, merge current `origin/main` into your branch.

Every pull request is a merge commit. Squash and rebase are off in this repository.

After editing the operator docs or `AGENTS.md`, run `./scripts/gen-llms.sh` and commit the result.

Local setup, tests, and CI are in [`docs/06-developers.md`](docs/06-developers.md).

## Issues

File a bug, a feature request, or a docs correction with the issue forms. Questions are not tracked as issues; the operator guide is in [`docs/`](docs/README.md).

An issue closes when the change is on `main`. The local rules and the setup checklist are in [`.ai-docs/issue-tracker.md`](.ai-docs/issue-tracker.md). Maintainers follow the practice in `Morpheus-Maintainer-Context` (`.ai-docs/how-we-work.md`).

The pull request body contains `Refs #123`, or the no-issue line for a chore, revert, or CI-only change. The **PR target** check fails a pull request that does not target `main`.
