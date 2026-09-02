# Slice 1: Initialize the repository and select the agent

Status: Ready for implementation
Depends on: None

## Problem Statement

The existing CLI only presents an agent selector. A repository user needs one clear initialization flow that selects the agent, records that choice, and safely installs the files required by the MVP while preserving every repository-owned customization.

## Solution

Move the Huh-based selector into `awdev init`, make the root command show help, add the source-aware command surface, and implement additive repository initialization. Record the selected provider, install embedded configuration, prompts, and result schemas without overwriting existing files, and validate schema-version-1 configuration before any operational side effect.

## User Stories

1. As a repository user, I want initialization to select and record my agent, so that repository setup is one coherent flow.
2. As a Codex user, I want selecting Codex to complete successfully, so that I can continue with the supported provider.
3. As a Claude Code user, I want a precise unavailable message, so that I understand the option is visible but not implemented.
4. As a repository maintainer, I want initialization to install all required defaults, so that later workflow phases have a complete contract.
5. As a repository maintainer, I want repeated initialization to be idempotent, so that rerunning it is safe.
6. As a repository maintainer, I want edited configuration and prompts preserved byte-for-byte, so that initialization never destroys local policy.
7. As a CLI user, I want source-aware help and validation, so that future sources can be added without changing the command grammar.
8. As an operator, I want invalid configuration rejected before external actions, so that bad policy cannot partially start a workflow.
9. As a parent workflow, I want nested worker invocation guarded, so that an agent cannot recursively start or resume orchestration.

## Implementation Decisions

- `awdev` without a subcommand shows help. `awdev init` starts with the form titled `Select the AI:` and keeps the Codex and Claude Code choices.
- Keep Huh as a runtime dependency. Claude Code selection prints exactly `Claude Code is not implemented yet.` and performs no Git, GitHub, agent, or state action.
- Add `init`, `run SOURCE NUMBER`, `status SOURCE NUMBER`, `resume SOURCE NUMBER`, and the later `retry SOURCE NUMBER` command shape to Cobra. Operational commands require discovery of an original Git repository root.
- Require `github` for source-scoped commands. Reject missing sources, legacy unscoped numbers, non-positive issue numbers, and extra arguments before repository or external work. A recognized `linear` source reports `Linear is not implemented yet.` without side effects.
- Refuse `run` and `resume` when `AWDEV_WORKER=1`; keep `status` read-only and available.
- Embed ordinary source assets for the default configuration, five prompts, and two JSON Schemas.
- Initialization is additive: create missing directories and files, retain existing files exactly, and append only missing ignore entries for issue state and worktrees without reordering unrelated content.
- Print only a concise `.awdev/` creation or retention message, `.gitignore` creation/update/retention information, and a final initialization-complete message. Do not list every installed file.
- Define strict schema-version-1 configuration for the selected agent provider, Codex binary, argument-array checks, positive agent and per-check duration strings, a review attempt cap from 1 through 10 with default 3, and optional protected path prefixes.
- Default the agent timeout to 30 minutes and each check timeout to 10 minutes. Load all installed assets relative to the controller root.
- Invoke child programs by executable and argument array, never through a shell command string.

## Testing Decisions

- The primary seam is the constructed Cobra command with injected input, output, repository discovery, and initialization services; tests assert user-visible behavior without launching real external tools.
- Cover root help plus init selector choices, cancellation, displayed options, exact Claude Code output, selected-provider persistence, concise summaries, and absence of side effects.
- Cover all argument errors, source-aware help, legacy command rejection, recognized-but-unimplemented sources, and worker guards.
- Use temporary real Git repositories to verify root discovery from root and nested directories.
- Use golden filesystem fixtures for the initialized layout, second-run idempotency, existing-file preservation, and summaries.
- Exercise ignore-file behavior for absent, partial, duplicate, and missing-final-newline inputs.
- Validate unknown fields, unsupported schema versions, empty commands, default and out-of-range review limits, default/zero/negative/malformed timeouts, and protected paths.

## Acceptance Criteria

- Plain `awdev` shows help and `awdev init` offers both agents through Huh before repository discovery or initialization.
- Codex selection is stored as `agent.provider: codex` in the installed configuration.
- Claude Code selection emits the exact unavailable message and has no operational side effects.
- Initialization produces the complete default layout and a second run changes nothing.
- Existing assets and unrelated ignore content are never overwritten or reordered.
- Help displays the exact source-aware forms, including `awdev run github NUMBER`.
- Invalid configuration fails before GitHub, worktree, state, check, or agent activity.

## Out of Scope

- Implementing Claude Code or Linear.
- Running an issue workflow.
- Overwriting or migrating existing repository policy.
- Installing the optional repository skill, which belongs to Slice 11.

## Further Notes

This slice establishes configuration version 1 with the selected provider and all fields required by later slices, avoiding a schema bump during the MVP. The highest useful verification seam is the CLI boundary plus a temporary repository because initialization is primarily observable through command output and filesystem state.
