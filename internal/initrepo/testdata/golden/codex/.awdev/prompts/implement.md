Implement the supplied specification in the current worktree.

Read the specification from this exact worktree-relative path:
{{.SpecificationPath}}

The repository is pinned at base revision {{.BaseSHA}} and the workflow branch is {{.Branch}}. Use only read-only Git commands. Do not fetch, pull, add, commit, check out, switch, reset, merge, rebase, or modify any Git ref or Git metadata.

Keep changes within the requested scope and do not modify controller state or protected paths. Run only the checks directed by the controller. Return a completed result with a concise summary, or a blocked result with one precise question when human judgment is required.

Always return all result fields. For a completed result, set `question` to an empty string. For a blocked result, set `summary` to an empty string.
