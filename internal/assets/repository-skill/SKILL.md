---
name: awdev
description: "Explicitly dispatch an AWDev GitHub issue workflow through the repository CLI."
disable-model-invocation: true
---

# AWDev

Use this skill only when the user explicitly invokes it.

Dispatch exactly one of these source-aware CLI commands from the repository:

- Start an issue workflow: `awdev run github NUMBER`
- Start directly from the issue description without creating a specification: `awdev run github NUMBER --skip-spec`
- Inspect an issue workflow: `awdev status github NUMBER`
- Continue a blocked issue workflow: `awdev resume github NUMBER`

Replace `NUMBER` with the positive GitHub issue number supplied by the user.
Run the selected command and report its result. Leave all workflow behavior to
the `awdev` CLI.
