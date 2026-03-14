# bugo

[![Docker](https://img.shields.io/badge/docker-ready-blue.svg)](https://www.docker.com/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Image Tags](https://ghcr-badge.yuchanns.xyz/yuchanns/bugo/tags?ignore=latest)](https://ghcr.io/yuchanns/bugo)
![Image Size](https://ghcr-badge.yuchanns.xyz/yuchanns/bugo/size)

`bugo` is an agent runtime in Go.

It is built for long-running operator-style use: explicit sessions, tape-backed memory, visible tool execution, and handoff-friendly continuity.
The current implementation supports Telegram as its first channel.

## Current Implementation

This repository contains the current Go runtime for `bugo`.
Today it is wired to Telegram as the active channel and uses `blades` as the agent runtime.

- Telegram adapter: `app.go`
- Tool registration: `app.go` + `model_tools.go`
- Local command execution: `commands.go`
- Session runtime: `session.go`
- Runtime internals: `internal/runtime`
- Workspace prompt loading: `workspace_prompt.go`

## Quick Start

```bash
go install github.com/yuchanns/bugo@latest
```

```bash
export BUGO_TELEGRAM_TOKEN="123456:xxxx"
export BUGO_API_KEY="sk-xxxx"
bugo
```

## Runtime Behavior

- Telegram private chats are active by default.
- Telegram group chats require mention or reply context.
- Inputs starting with `,` are treated as local runtime commands.
- Regular turns run through the agent loop with tool support.
- Session memory is stored in tape and can be retrieved through tape tools.
- Assistant replies stream through Telegram drafts and finalize as normal messages.
- If a user sends more input while a run is still iterating, the runtime can inject that input into the next model iteration instead of waiting for the whole run to finish.

## Commands

Examples:

```text
,help
,tools
,tool.describe name=fs.read
,fs.read path=README.md
,fs.write path=notes/todo.txt content="hello"
,fs.edit path=notes/todo.txt old=hello new=world
,tape.info
,tape.search query=error
,tape.handoff summary="checkpoint"
,schedule.list
,skills.list
```

## Skills

- Built-in skills are embedded from `skills/`
- External skills are loaded from `<workspace>/.agents/skills`
- External skills override built-ins with the same name
- Each skill directory must include `SKILL.md`

## Runtime Environment Variables

- `BUGO_TELEGRAM_TOKEN`: Telegram bot token
- `BUGO_API_KEY`: model provider key
- `BUGO_API_BASE`: optional provider base URL
- `BUGO_MODEL`: model name, default `gpt-4o-mini`
- `BUGO_MAX_ITERATIONS`: max agent iterations
- `BUGO_MAX_OUTPUT_TOKENS`: max model output tokens
- `BUGO_PROMPT_TOKEN_LIMIT`: optional soft prompt budget for context-pressure hints
- `BUGO_TELEGRAM_ALLOW_CHATS`: allowed chat ids
- `BUGO_TELEGRAM_ALLOW_FROM`: allowed user ids or usernames
- `BUGO_WORKDIR`: workspace root
- `BUGO_HOME`: runtime home, default `~/.bugo`

## Storage

- Tape files: `~/.bugo/tapes/*.jsonl`
- Each session maps to one append-only JSONL tape

## Development

```bash
go mod tidy
go test ./...
go build -o bugo .
```

## License

[Apache-2.0](./LICENSE)

## Credits

- Inspired by [bub](https://github.com/bubbuild/bub)
