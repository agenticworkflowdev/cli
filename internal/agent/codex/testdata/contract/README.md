# Codex exec contract evidence

This directory records the disposable contract spike requested by issue #6.
The observations came from the real installed Codex executable rather than a
fake process. They are evidence for adapter tests, not a second implementation
of the production adapter.

## Capture

- Recorded: 2026-09-03T13:44:56Z
- Codex: `codex-cli 0.146.0`
- Platform: `darwin/arm64`
- Invocation: `codex exec --json --color never --sandbox <mode>
  --output-schema <absolute path> --cd <absolute worktree>
  --output-last-message <controller-owned path> -`
- Prompt channel: stdin, selected by the final `-` argument
- Environment shape: only the keys recorded in `manifest.json`; values were
  never written to the fixtures

The harness uses disposable Git repositories and publishes only sanitized
stdout, stderr, final output, and observations. `$CODEX`, `$SCHEMA`,
`$WORKSPACE`, `$FINAL_OUTPUT`, `$SPIKE_ROOT`, and `$HOME` are redaction labels,
not literal paths from the run.

## Observed contract decisions

1. `--json` writes newline-delimited progress objects to stdout. A successful
   run started with `thread.started`; its `thread_id` was the session identity.
   Subsequent event payloads were additive and included `item.started` and
   `item.completed`, so the adapter must tolerate event and item details it does
   not understand.
2. `--output-last-message` wrote only the final schema-constrained assistant
   object to the controller-created file. The same text also appeared inside an
   `item.completed` event, but the file is the authoritative result channel.
   Failed and canceled runs left the file empty.
3. The read-only probe emitted a real `command_execution` event. Its attempted
   Python write failed with `PermissionError: [Errno 1] Operation not permitted`,
   exited 1, and the independent post-run check found no marker. The
   workspace-write command exited 0 and created a marker with the exact requested
   bytes. The `--sandbox` values therefore mapped to the expected observed
   filesystem restrictions in these workspaces.
4. A schema that omitted the required array was rejected by the API. Codex
   emitted `error` and `turn.failed`, exited with code 1, and produced no final
   output. An adapter must not trust an empty or stale final-output target after
   a process failure.
5. The large-output probe produced one 131,275-byte JSONL event. This exceeds
   the 64 KiB default token limit of Go's `bufio.Scanner`; JSON streaming or an
   explicitly enlarged limit is required.
6. Canceling after five seconds preserved the preceding `thread.started` and
   `turn.started` events and left final output empty. The process runner returned
   a deadline error even though the terminated Codex parent reported exit code
   0. Cancellation classification must therefore prefer the context error over
   the numeric exit code.
7. No unknown top-level event type was emitted during these six runs. The
   recorded additive item payloads demonstrate why item details stay opaque;
   `../oversized-unknown.jsonl` remains the forward-compatibility fixture for a
   genuinely unknown top-level event.
8. Non-empty stderr did not imply run failure. User-configured MCP startup
   emitted unrelated authentication diagnostics during successful runs, so
   status must come from process/context state and the final-output contract.

These observations describe this exact Codex version and platform. A future
binary may intentionally change them; refresh the evidence and review the diff
before changing adapter behavior.

## Fixture layout

Each scenario directory contains:

- `events.jsonl`: complete sanitized stdout from `--json`;
- `stderr.txt`: complete sanitized stderr;
- `observation.json`: arguments, working directory label, environment keys,
  exit/cancellation state, event types, sizes, and marker postconditions;
- `final-output.json`: the non-empty controller-owned final output, present only
  for runs that produced one.

`manifest.json` identifies the executable and platform and repeats the compact
scenario observations. Repository tests replay every event and final output,
assert the contract boundaries above, and scan all evidence for credential
patterns, user-specific paths, and the harness's deliberate prompt sentinel.

## Reproducing the spike

Run the harness only when intentionally refreshing this evidence:

```sh
go run ./tools/codex-contract-spike \
  -output ./codex-contract-evidence-new
```

The output directory must not already exist. Review the new sanitized evidence
before deliberately replacing these fixtures. Missing Codex, unavailable
authentication, or output truncation makes the harness fail and is a spike
blocker rather than evidence about the adapter contract.
