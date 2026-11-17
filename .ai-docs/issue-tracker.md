# Issue tracker

A change is done when it is on `main`. This repository has no `dev` or `test` branch. A merge to `main` that touches a material path publishes a release.

Maintainers follow the full practice in the private repo `MorpheusAIs/Morpheus-Maintainer-Context`, file `.ai-docs/how-we-work.md`. This page is the local rules for this repository.

## Filing

Use the issue forms:

- **Bug report** for something broken
- **Feature request** for a change or a proposal
- **Docs** for a wrong or missing page in the operator guide, the README, or `AGENTS.md`

Questions are not tracked as issues. The operator guide is in [`docs/`](../docs/README.md). Blank issues are closed.

Security issues are never a public issue. Use the contact in [DISCLAIMER.md](../DISCLAIMER.md#contact).

A new issue is labeled `needs-triage`. During triage, set one area label and the Priority field, set the status on the **Morpheus Work** project, then clear `needs-triage`.

Area labels: `area: gateway`, `area: sessions`, `area: keys`, `area: gui`, `area: deploy`, `area: docs`.

Priority is the GitHub field in the issue sidebar: **Urgent** now, **High** next, **Medium** backlog, **Low** someday. The issue form sets Type (Bug, Feature, Task). Effort is optional.

## Pull requests

Every pull request targets `main`.

The body contains a line `Refs #123`, or the checked no-issue line for a chore, revert, or CI-only change.

Every pull request is a merge commit. Squash and rebase are off.

When this work is part of an initiative, create the issue as a sub-issue of that initiative:

```shell
gh issue create --repo MorpheusAIs/Morpheus-Uplink \
  --title "TITLE" --body "BODY" \
  --parent https://github.com/MorpheusAIs/Morpheus-Maintainer-Context/issues/N
```

## Status

The public project **Morpheus work** holds issues from this repo. Status for this repo:

Triage → Needs Info → Backlog → Ready → In Progress → Shipped

Shipped means the commit is on `main`. The workflow comments on the linked issue when the pull request opens, and comments and closes it when the pull request merges.

`Refs #` on the **pull request body** are authoritative. Commit messages in the pull request may also carry `Refs #`.

## The workflow and the build

`.github/workflows/issue-tracker.yml` runs on pull request events only. It does not build, tag, or publish.

The tracker files are not material paths, so a pull request that changes only them does not run the build and does not cut a release.

`.github/scripts/issue-tracker.js` is the Morpheus-Lumerin-Node script with one addition: the branch feature pull requests target is set by `FEATURE_BASE` (here `main`). It is the same file as in Morpheus-Infra.

## Setup checklist

In the repository:

- [x] Issue forms for bug, feature, and docs
- [x] Pull request template asks for `Refs #` or the no-issue line
- [x] `CONTRIBUTING.md` states that pull requests target `main`
- [x] Workflow fails a pull request that does not target `main`
- [x] Workflow comments on the linked issue and closes it on `main`
- [x] Labels created: `needs-triage`, `needs-info`, `needs-issue`, and the `area:` labels
- [x] Public project **Morpheus Work** is https://github.com/orgs/MorpheusAIs/projects/3
- [x] Repository variable `MORPHEUS_WORK_PROJECT` is `3`
- [x] Organization secret `MORPHEUS_WORK_TOKEN` is granted to this repository. It is a fine-grained token with organization Projects read and write, and repository Issues and Pull requests read and write. Without it the workflow still comments and closes, and skips the status move.
- [x] The project's auto-add workflow includes this repository, so a new issue arrives in Triage
- [ ] Open issues added to the project in Triage
- [ ] The **PR target** check is required on `main`
- [ ] Later, the missing-issue reminder becomes a required check
