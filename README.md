# AWDev CLI

AWDev runs one GitHub issue through specification, implementation, deterministic
checks, independent review, and pull-request creation. It runs in the
foreground and keeps durable state in the repository so a blocked or
interrupted workflow can be inspected safely.

## Prerequisites

- Go 1.26.6 or newer to build from source.
- Git, with a non-shallow local checkout and an `origin` remote that identifies
  the same GitHub repository selected by `gh`.
- [GitHub CLI](https://cli.github.com/) installed and authenticated for the
  repository. AWDev uses the active `gh` identity; `GH_TOKEN` can select a
  non-interactive identity.
- Codex CLI installed and authenticated. Its executable can be changed in
  `.awdev/config.json`.

AWDev invokes GitHub CLI and Codex directly with argument arrays. It does not
run either command through a shell, and GitHub prompts are disabled.

## Initialize and run

From anywhere inside the repository, initialize AWDev and choose Codex:

```sh
awdev init
```

Initialization adds missing defaults under `.awdev/` and adds the controller
state and worktree entries to `.gitignore`. Existing configuration, prompts,
schemas, and unrelated ignore entries are retained.

To also install the explicit-only repository `$awdev` skill, opt in during
initialization:

```sh
awdev init --with-skill
```

The flag additively installs `.agents/skills/awdev/SKILL.md` and its Codex
metadata. Repeating it fills in missing files without changing existing ones;
plain `awdev init` does not install or require the skill.

Run an issue and inspect its stable machine-readable status:

```sh
awdev run github 123
awdev status github 123
awdev status github 123 --json
```

If an agent needs a decision, AWDev posts one marked issue comment and stops in
`blocked` state. Reply to that comment with a GitHub user account, then run:

```sh
awdev resume github 123
```

`resume` always starts a fresh agent process for the recorded phase. If final
pull-request publication failed after review, correct the external cause and
use the deliberately narrow retry command:

```sh
awdev retry github 123
```

The process owns the workflow lock while it runs. Pressing Ctrl-C cancels the
active child process tree. AWDev does not continue in a daemon after the
foreground command exits.

## Configuration

`.awdev/config.json` uses schema version 1. Its fields are:

- `agent.provider`: `codex` for the current release.
- `agent.timeout`: positive Go duration applied to each agent invocation; the
  shipped default is `30m`.
- `codex.binary`: executable name or path for Codex CLI.
- `checks`: ordered checks, each with a name, optional repository-relative
  directory, argument-array command, and positive timeout. The shipped check
  timeout is `10m`.
- `review.max_attempts`: independent review attempts, from 1 through 10; the
  shipped default is 3.
- `protected_paths`: additional repository-relative prefixes an agent may not
  change. AWDev always protects controller state and Git metadata regardless
  of this list.

Checks run in the generated worktree with credential variables removed. A
failed implementation check can receive up to three automatic repair attempts;
the configured review cap controls review/correction attempts.

## Prompt templates

`awdev init` installs six editable templates in `.awdev/prompts/`: specification,
implementation, review, check repair, review repair, and blocked-workflow
continuation. They discover the target repository's languages, architecture,
and tools instead of assuming frontend/backend folders or a particular stack.
See [prompt template guidance](docs/prompt-templates.md) for their responsibilities,
inputs, and customization rules.

Initialization preserves existing templates. Updating the CLI does not overwrite
prompts already installed in a repository; compare and merge the new defaults
manually when upgrading. Configure meaningful project checks in
`.awdev/config.json`: the shipped whitespace check does not verify behavior.

## State, failures, and recovery

Controller state remains in the original checkout:

- `.awdev/issues/<workflow-id>/manifest.json` is the durable workflow record.
- `.awdev/issues/<workflow-id>/review.json` is current review evidence when
  present.
- `.awdev/logs/` contains private diagnostic logs for command failures.
- `.awdev/worktrees/gh-<number>` is the isolated real Git worktree.

The issue title and body are snapshotted before the initial manifest is saved;
later edits do not silently change workflow input.
`awdev status github 123 --check-issue` can report that the live issue has
changed, while `awdev status github 123 --json` remains the stable automation
contract.

After durable state exists, technical failures are recorded with the current
phase, `status: failed`, and `last_error`. Human questions use `status:
blocked`. State, worktrees, and diagnostics are preserved. Only
`pull_request/failed` supports `retry`; see [manual recovery](docs/manual-recovery.md)
for phase-specific guidance.

## Current boundaries

GitHub is the implemented issue source and Codex is the implemented agent.
Claude Code remains visible in the initialization selector but selecting it
only reports that it is unavailable. The source-aware command grammar reserves
`linear` for a future source, but Linear operations are unavailable.

This MVP creates one pull request. It does not wait for hosted CI, merge pull
requests, clean up worktrees, run hosted workflows, or expose an MCP service.
