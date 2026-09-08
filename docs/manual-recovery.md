# Manual workflow recovery

AWDev deliberately limits `awdev retry github N` to workflows in
`pull_request/failed`. This prevents a retry command from skipping an approval,
duplicating external state, or publishing an unverified branch.

Before intervening, run `awdev status github N` and preserve the workflow's
`.awdev` state and worktree. Do not edit the manifest, delete the worktree, move
the recorded branch, or close the source issue to force progress; those actions
remove evidence AWDev uses for safe reconciliation.

Inspect `.awdev/issues/<workflow-id>/manifest.json`, especially `phase`,
`status`, `last_error`, `worktree`, `branch`, `base_sha`, `commit_tree_sha`,
`commit_sha`, `blocker`, and `pull_request`. Diagnostic logs in `.awdev/logs/`
may be copied elsewhere and removed after investigation; removing them does not
change workflow state. Do not remove manifest or review files.

Use the recovery boundary for the recorded phase:

- Before an initial manifest exists, fix repository discovery, configuration,
  GitHub authentication, issue validity, or the reported worktree collision and
  rerun `awdev run github N`. AWDev adopts only an exact validated bootstrap
  worktree; it never repairs a conflicting path or branch destructively.
- For `spec/failed`, preserve the generated worktree and error log. Correct an
  external Codex, timeout, schema, or filesystem problem, then inspect the
  partial spec if one exists. There is no automated retry for this phase.
- For `implementation/failed`, inspect `last_error` and the private log for
  agent and check results. Fix the environment rather than bypassing a check.
  There is no automated retry after the bounded check-repair loop.
- For `review/failed`, preserve `review.json` when present and inspect whether
  the failure came from the reviewer, a correction, or rerun checks. There is
  no automated retry for this phase.
- For `pull_request/failed`, correct the reported external problem (for example,
  authentication or network access), then run `awdev retry github N`. AWDev
  reruns final checks, reconciles the recorded commit and branch, pushes again,
  and reuses an existing matching pull request when present.
- For `pull_request/running`, run `awdev run github N`. Re-entry reconciles the
  interrupted publication while holding the normal workflow lock.
- For a blocked workflow with a published question, answer it on GitHub and run
  `awdev resume github N`. Repeating `run` does not publish another blocker.

For failed phases other than pull-request publication, preserve the evidence,
fix only the reported environmental cause, and escalate for manual inspection
rather than rewriting durable state or publishing Git state by hand.

If the workflow is being abandoned, first copy any wanted uncommitted work and
verify that the manifest's `worktree` and `branch` match Git's registered
worktree. From the controller checkout, remove only that exact recorded path
with `git worktree remove PATH` without `--force`. Git will refuse removal when
uncommitted work remains. Branch deletion is a separate manual decision; never
delete or reset the controller checkout or a path inferred only from an issue
number.

AWDev recovery never waits for CI, merges a pull request, directly closes the
issue, or cleans up the worktree.
