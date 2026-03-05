---
name: telegram
description: Send Telegram messages with a script fallback when tool calls need custom formatting.
allowed-tools: "telegram_send,tape_*"
---

Use this skill when you need to push a message to Telegram.

Preferred path:
1. Use tool `telegram_send` directly when possible.

Fallback path:
1. Use `run_skill_script` with `skill_name="telegram"` and `script_path="scripts/send.sh"`.
2. Pass arguments:
   - `--chat-id <chat_id>`
   - `--text <message>`
3. Script reads token from `BUGO_TELEGRAM_TOKEN`.

Example:
`run_skill_script` with args:
`["--chat-id", "123456", "--text", "Task finished."]`
