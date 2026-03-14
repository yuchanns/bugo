---
name: bash
description: Run arbitrary bash commands inside the current workspace through the skill runtime. Use when the task needs shell commands such as git, rg, go test, ls, cat, jq, or other command-line tools and you want to execute them through run_skill_script.
metadata:
  short-description: Run bash commands in workspace
---

# Bash

Use this skill when shell execution is needed.

## Workflow

1. Use `run_skill_script`.
2. Set `skill_name` to `bash`.
3. Set `script_path` to `scripts/run.sh`.
4. Pass the shell command in env var `BUGO_CMD`.
5. Pass the current workspace root in env var `BUGO_WORKSPACE`.
6. Optionally set `timeout_seconds` when a command may run longer than the default.

## Required env

- `BUGO_WORKSPACE`: absolute workspace path
- `BUGO_CMD`: bash command to execute

## Notes

- The script changes directory to `BUGO_WORKSPACE` before running the command.
- Commands run as `bash --noprofile --norc -lc "$BUGO_CMD"`.
- Prefer concise read-only commands when inspection is enough.
- Use longer timeouts only when the task clearly needs them.
