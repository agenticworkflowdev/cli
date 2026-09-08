# Manual workflow recovery

AWDev deliberately limits `awdev retry github N` to workflows in
`pull_request/failed`. This prevents a retry command from skipping an approval,
duplicating external state, or publishing an unverified branch.

Before intervening, run `awdev status github N` and preserve the workflow's
`.awdev` state and worktree. Do not edit the manifest, delete the worktree, move
the recorded branch, or close the source issue to force progress; those actions
remove evidence AWDev uses for safe reconciliation.

- For `pull_request/failed`, correct the reported external problem (for example,
  authentication or network access), then run `awdev retry github N`. AWDev
  reruns final checks, reconciles the recorded commit and branch, pushes again,
  and reuses an existing matching pull request when present.
- For `pull_request/running`, run `awdev run github N`. Re-entry reconciles the
  interrupted publication while holding the normal workflow lock.
- For a blocked workflow with a published question, answer it on GitHub and run
  `awdev resume github N`.
- Other failed phases have no automated retry path. Preserve the diagnostics and
  worktree, fix only the reported environmental cause, and escalate for manual
  inspection rather than rewriting durable state or publishing Git state by
  hand.

AWDev recovery never waits for CI, merges a pull request, directly closes the
issue, or cleans up the worktree.
