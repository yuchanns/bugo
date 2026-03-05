# bugo

`bugo` is a Telegram agent runtime in Go, inspired by and implemented with reference to:

- https://github.com/bubbuild/bub/

Current scope includes:

- Telegram polling adapter (`go-telegram/bot`)
- Session-based execution (`blades.Runner` + custom `Session`)
- Append-only tape storage (JSONL)
- Skills loading (`embed.FS` built-ins + optional external overrides)

## 1. Install

```bash
go install github.com/yuchanns/bugo@latest
```

After installation, run:

```bash
bugo
```

## 2. Required environment variables

```bash
export BUGO_TELEGRAM_TOKEN="123456:xxxx"
export BUGO_API_KEY="sk-xxxx"
```

Optional:

```bash
export BUGO_MODEL="gpt-4o-mini"
export BUGO_API_BASE="https://openrouter.ai/api/v1"
export BUGO_PROACTIVE_RESPONSE=false
export BUGO_TELEGRAM_ALLOW_CHATS='["123456789"]'
export BUGO_TELEGRAM_ALLOW_FROM='["123456789","your_username"]'
export BUGO_HOME="~/.bugo"
```

Notes:

- If `OPENROUTER_API_KEY` is set and `BUGO_API_BASE` is empty, `BUGO_API_BASE` defaults to `https://openrouter.ai/api/v1`.
- `BUGO_TELEGRAM_ALLOW_CHATS` and `BUGO_TELEGRAM_ALLOW_FROM` accept either JSON array or comma-separated values.

## 3. Skills loading

### 3.1 Built-in skills (default)

Repository `skills/` are embedded into the binary via `embed.FS` and loaded by default.
Current built-ins include:

- `telegram`
- `skill-creator`
- `skill-installer`

### 3.2 External skills (optional override)

```bash
export BUGO_EXTRA_SKILLS_DIR="/path/to/skills"
```

If an external skill has the same name as a built-in one, the external skill overrides it.

## 4. Telegram behavior

- Private chats are handled by default.
- Group chats are handled only when mention/reply conditions match.
- Session key format: `telegram:<chat_id>`.
- Inputs with `,` prefix go to local command channel.
- `BUGO_PROACTIVE_RESPONSE=false`: agent text is auto-sent.
- `BUGO_PROACTIVE_RESPONSE=true`: assistant text is not auto-sent (tool/skill should send explicitly).

Command examples:

```text
,help
,tools
,tool.describe name=fs.read
,git status
,fs.read path=README.md
,fs.write path=notes/todo.txt content="hello"
,fs.edit path=notes/todo.txt old=hello new=world
,web.fetch url=https://example.com
,web.search query=golang
,tape.handoff name=phase-1 summary="bootstrap done"
,tape.anchors
,tape.info
,tape.search query=error
,tape.reset archive=true
,schedule.add cron="*/5 * * * *" message="ping"
,schedule.list
,schedule.remove job_id=my-job
,skills.list
,skills.reload
,quit
```

## 5. Tape storage

Default location:

```text
~/.bugo/tapes/*.jsonl
```

Each session maps to one append-only JSONL tape file.

## 6. Local development

```bash
go mod tidy
go mod vendor
go test ./...
go build -o bugo .
```
