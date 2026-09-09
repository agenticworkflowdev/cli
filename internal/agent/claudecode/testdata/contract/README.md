# Claude Code contract evidence

This directory records the disposable contract spike requested by issue #13
(wave B). The observations came from the real installed Claude Code executable
rather than a fake process. They are evidence for adapter tests, not a second
implementation of the production adapter. The layout deliberately mirrors
`internal/agent/codex/testdata/contract/`.

## Capture

- Recorded: 2026-09-09T07:08:56Z
- Claude Code: `2.1.266 (Claude Code)`
- Platform: `linux/arm64`
- Invocation: `claude --print --output-format stream-json --verbose
  --json-schema <inline schema string> --permission-mode <plan|acceptEdits>
  --permission-prompts none --model claude-haiku-4-5-20251001
  --session-id <controller-generated UUID> --add-dir <worktree>`
- Prompt channel: stdin. No positional prompt argument is passed.
- Working directory: the child process cwd is the throwaway worktree. There is
  no `--cd` / `--cwd` flag; the directory is set on the process. `--add-dir
  <worktree>` is passed for parity with the planned adapter even though it is
  redundant when cwd already is the worktree.
- Model: `claude-haiku-4-5-20251001` everywhere, with tiny prompts, to keep the
  spike cheap.
- Environment: a clean allowlist. Only the key names recorded in
  `manifest.json` (`AWDEV_WORKER`, `CLAUDE_CONFIG_DIR`, `HOME`, `PATH`) were
  passed to the child; values were never written to any fixture. The allowlist
  the harness offers also includes `CLAUDE_CODE_OAUTH_TOKEN` and
  `ANTHROPIC_API_KEY` when present.

Identity in this capture environment is a `claude.ai` OAuth subscription token
(`"apiKeySource":"none"` in the init event, no `ANTHROPIC_API_KEY`). Auth
succeeded and every scenario except the deliberately malformed one produced a
live result, so these are real contract fixtures. `--bare` is never used: it
only reads `ANTHROPIC_API_KEY` and would fail an OAuth identity. If this
evidence is refreshed in an environment that authenticates a different way, the
`environment_keys` and the auth notes above will need updating, but the stream
contract itself should not move.

## Sanitization

The harness publishes only sanitized `events.jsonl`, `stderr.txt`,
`final-output.json`, and `observation.json` per scenario. The following are
redaction labels, not literal values from the run:

| Label | Replaces |
|---|---|
| `$CLAUDE` | absolute path to the `claude` executable |
| `$SCHEMA` | the inline `--json-schema` string (argv only) |
| `$WORKSPACE` | the disposable spike root and worktree paths, both `/`-joined and `-`-slugified forms |
| `$HOME` | `$HOME`, both `/`-joined and slugified forms |
| `$CONFIG_DIR` | `CLAUDE_CONFIG_DIR`, both `/`-joined and slugified forms |
| `$SESSION` | the controller-generated `--session-id` UUID (which the CLI echoes on every event) |
| `$RUNTIME` | the CLI's `/tmp/*cc-socks*` messaging socket path |
| `[PROMPT_REDACTED]` | the harness's deliberate per-prompt trace sentinel |
| `[CREDENTIAL_REDACTED]` | credential-shaped strings and any live token value |

After writing every fixture the harness re-scans the whole tree for absolute
home-directory and user-profile paths, credential-shaped strings, the prompt
sentinel, and the live token value, and refuses to publish the evidence
directory if any is found. The thinking-block `signature` blobs in
`events.jsonl` are opaque per-response reasoning-integrity data, not
credentials, and are kept verbatim as part of the real stream.

## Observed contract decisions

1. `--output-format stream-json --verbose --print` writes newline-delimited
   JSON events to stdout. The first event is
   `{"type":"system","subtype":"init"}` and carries `session_id`, `cwd`,
   `model`, `permissionMode`, and a `tools` array that includes the synthetic
   `StructuredOutput` tool whenever `--json-schema` is set. `session_id` is
   repeated on every subsequent event and on the terminal result; there is no
   `thread.started` analogue, so the controller owns identity by passing
   `--session-id`. `session_id_consistent` is `true` in every non-trivial
   scenario.
2. The distinct top-level event `type`s seen across these runs are `system`,
   `rate_limit_event`, `assistant`, `user`, and `result` (recorded per scenario
   in `event_types`). `system` carries `subtype` values `init`,
   `thinking_tokens`, and — when a tool is auto-denied — `permission_denied`.
   `rate_limit_event` and `system/thinking_tokens` are progress noise with no
   provider-neutral `Kind`. An adapter must tolerate event types, subtypes, and
   payload fields it does not understand.
3. With `--json-schema` the model calls the synthetic `StructuredOutput` tool
   and the terminal `{"type":"result"}` event carries the object in BOTH
   `structured_output` (parsed object) and `result` (the same object as a JSON
   string); `stop_reason` becomes `"tool_use"`. `final-output.json` here is the
   pretty-printed `structured_output`, written only for runs that produced one.
   There is no side file — `--output-last-message` has no analogue — so the
   terminal event is the only result channel.
4. A malformed `--json-schema` string is rejected by the CLI before any API
   call: exit 1, a single `Error: --json-schema is not valid JSON: ...` line on
   stderr, empty stdout, no events, no session. This is the `schema-rejected`
   scenario. A well-formed schema cannot be "violated" through the tool, so
   there is no schema-violation event to capture; the adapter's real failure
   mode is a run that omits `structured_output` entirely (see decision 10).
   Separately, the CLI rejects a schema that carries a `$schema` dialect URI it
   cannot fetch — the repository's own `agent-result.schema.json` fails with
   `Error: --json-schema is not a valid JSON Schema: no schema with key or ref
   "https://json-schema.org/draft/2020-12/schema"`. Confirmed against a live
   `claude` run. The adapter therefore strips the informational `$schema` and
   `$id` keys before passing the schema (`structuredOutputSchema`); Claude Code
   validates the object shape only, so nothing meaningful is lost.
5. `status:"blocked"` is an ordinary `structured_output` payload, not a
   process-level signal: exit 0, `is_error:false`, a normal terminal `result`
   event, `structured_output.status == "blocked"`. The adapter classifies
   blocked-vs-completed from the decoded object, never from the process.
6. `--permission-mode plan --permission-prompts none` behaves as read-only.
   `Read` runs (the `large-event` scenario reads a 200 KB file in plan mode).
   A write — here `Bash` output redirection — is auto-denied: the stream gets a
   `{"type":"system","subtype":"permission_denied"}` event
   (`decision_reason_type: "asyncAgent"`, "no approval surface in this
   session") and the denial is also listed in `permission_denials[]` on the
   terminal result. The denied call does NOT abort the run: it still exits 0
   with `is_error:false` and a terminal result. The independent post-run marker
   check (`workspace_marker_present: false`) is the authoritative filesystem
   evidence. `permission_denials` on the result is the in-band signal; neither
   the exit code nor `is_error` reflects a blocked tool.
7. `--permission-mode acceptEdits --permission-prompts none` permits edits and
   file-redirecting `Bash` inside the worktree. The `workspace-write` scenario's
   post-run check found the marker with the exact requested bytes
   (`workspace_marker_matched: true`) and `permission_denials` was empty.
8. Plan mode also injects a planning-workflow system prompt. Against a bare
   "return this structured object" instruction with no concrete task, the model
   sometimes refuses to call `StructuredOutput` at all (exit 0, `is_error:false`,
   no `structured_output`). The `successful-structured` and `blocked` scenarios
   are therefore phrased as genuine questions, and `successful-structured` runs
   under `acceptEdits`. The workflow prompts the adapter will actually send are
   concrete tasks, but this is a real risk for the spec / review phases, which
   the plan maps to plan mode — treat a missing `structured_output` as a run
   failure and consider hardening those prompts.
9. `SIGTERM` mid-run (the `cancellation` scenario, ~5 s deadline, 3 s kill
   grace) truncates the stream with NO terminal `result` event and no
   structured output, exit 143. There is nothing to salvage. Cancellation must
   be classified from the context error (the process runner returns a
   `context.DeadlineExceeded`), not from the exit code — "stream ended without a
   result event" is the tell.
10. The `large-event` scenario's `Read` tool_result is a single NDJSON `user`
    event of 129,453 bytes — well over `bufio.Scanner`'s 64 KiB default token
    size. JSON streaming or an explicitly enlarged buffer is required, and the
    adapter's stdout cap should match the planned 32 MiB limit.
11. A clean run leaves stderr empty in every scenario here. Non-empty stderr is
    CLI-level diagnostics only (the malformed-schema `Error:` line; in other
    environments an unknown-model line or a benign missing-`~/.claude.json`
    note). Status must come from process / context state plus the terminal
    `result` event, never from stderr being non-empty. `subtype` is `"success"`
    even on API errors and must not be trusted; `is_error`, `terminal_reason`,
    and the exit code are the reliable triad.

These observations describe this exact Claude Code version and platform. A
future binary may intentionally change them; refresh the evidence and review the
diff before changing adapter behavior.

## Fixture layout

Each scenario directory contains:

- `events.jsonl`: complete sanitized stdout (the NDJSON event stream);
- `stderr.txt`: complete sanitized stderr;
- `observation.json`: sanitized argv, working-directory label, environment key
  names, permission mode, exit / cancellation state, the event and
  system-subtype catalogue, byte sizes, the terminal-result shape
  (`result_is_error`, `result_terminal_reason`, `structured_output_present`,
  `structured_output_status`, `permission_denials`, `session_id_consistent`),
  and the independent workspace-marker postconditions;
- `final-output.json`: the pretty-printed `structured_output` object, present
  only for runs that produced one.

`manifest.json` identifies the executable and platform, records the exact
invocation template and environment key names, and repeats the compact
per-scenario observations. An `observation.json` may carry a
`validation_error` string when a live run did not match its recorded
expectation and the fixture needs re-capture; none of the current fixtures
carry one.

## Reproducing the spike

Run the harness only when intentionally refreshing this evidence:

```sh
go run ./tools/claude-code-contract-spike -output ./claude-code-contract-evidence-new
```

The output directory must not already exist. The harness needs the real
`claude` binary on `PATH` and a working non-`--bare` authentication. Missing
`claude`, unavailable authentication, or output truncation makes the harness
fail and is a spike blocker rather than evidence about the adapter contract.
Review the new sanitized evidence before deliberately replacing these fixtures.
