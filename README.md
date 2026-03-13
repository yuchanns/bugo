# bugo

[![Docker](https://img.shields.io/badge/docker-ready-blue.svg)](https://www.docker.com/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Image Tags](https://ghcr-badge.yuchanns.xyz/yuchanns/bugo/tags?ignore=latest)](https://ghcr.io/yuchanns/bugo)
![Image Size](https://ghcr-badge.yuchanns.xyz/yuchanns/bugo/size)

`bugo` is a Telegram agent runtime in Go, inspired by and implemented with reference to:

- https://github.com/bubbuild/bub/

Current scope includes:

- Telegram polling adapter (`go-telegram/bot`)
- Session-based execution (`blades.Runner` + custom `Session`)
- Append-only tape storage (JSONL)
- Skills loading (`embed.FS` built-ins + optional external overrides)

## 1. Recommended deployment (container, safer)

Container deployment is recommended for production usage.
It is safer than running directly on the host because process/runtime boundaries are isolated and writable paths are explicit.

The published image is intended to be agent-ready instead of a bare runtime.
It includes common CLI tools (`git`, `curl`, `jq`, `ripgrep`, `less`, `openssh-client`), Node.js tooling (`node`, `npm`, `npx`/`npm exec`), and a preinstalled virtual X server stack.
By default the container exports `DISPLAY=:99`, starts `Xvfb` before `bugo`, and keeps browser/runtime libraries available for visual automation workloads.
The runtime user defaults to a regular `bugo` account instead of `root`; it still has passwordless `sudo`, so the agent can self-manage packages or escalate when needed.

```bash
docker run -d --name bugo \
  -e BUGO_TELEGRAM_TOKEN="123456:xxxx" \
  -e BUGO_API_KEY="sk-xxxx" \
  -e BUGO_MODEL="gpt-4o-mini" \
  -v "$HOME/.bugo:/data/.bugo" \
  ghcr.io/yuchanns/bugo:latest
```

Optional hardening:

- Use a pinned image tag instead of `latest`.
- Use `--restart unless-stopped`.
- Mount only the data directory you need.

Virtual display knobs:

- `DISPLAY`: defaults to `:99`.
- `BUGO_ENABLE_XVFB=0`: disable auto-started virtual display when you want to provide your own X server.
- `XVFB_RESOLUTION=1920x1080x24`: override the default screen geometry/depth.
- `XVFB_ARGS="-dpi 120"`: append extra `Xvfb` flags when needed.

Container user knobs:

- Build args `BUGO_UID` / `BUGO_GID`: default to `1000`, useful when matching host-mounted volume ownership.
- The container starts as user `bugo`, but `sudo` does not require a password.
- This improves non-root runtime ergonomics, but it is not a hard security boundary because passwordless `sudo` still grants full root inside the container.

## 2. Install (local binary)

```bash
go install github.com/yuchanns/bugo@latest
```

After installation, run:

```bash
bugo
```

## 3. Required environment variables

```bash
export BUGO_TELEGRAM_TOKEN="123456:xxxx"
export BUGO_API_KEY="sk-xxxx"
```

Optional:

```bash
export BUGO_MODEL="gpt-4o-mini"
export BUGO_API_BASE="https://openrouter.ai/api/v1"
export BUGO_TELEGRAM_ALLOW_CHATS='["123456789"]'
export BUGO_TELEGRAM_ALLOW_FROM='["123456789","your_username"]'
export BUGO_BASH_ALLOW_ENV='["SSH_AUTH_SOCK","HTTP_PROXY","HTTPS_PROXY","NO_PROXY"]'
export BUGO_WORKDIR="/path/to/workspace"
export BUGO_HOME="~/.bugo"
```

Notes:

- If `OPENROUTER_API_KEY` is set and `BUGO_API_BASE` is empty, `BUGO_API_BASE` defaults to `https://openrouter.ai/api/v1`.
- `BUGO_TELEGRAM_ALLOW_CHATS` and `BUGO_TELEGRAM_ALLOW_FROM` accept either JSON array or comma-separated values.
- `BUGO_BASH_ALLOW_ENV` appends env names to the shell-tool inherit whitelist (JSON array or comma-separated).
- Default inherited shell env includes `PATH`, locale/time vars, and display-related vars such as `DISPLAY`, `XAUTHORITY`, and `XDG_RUNTIME_DIR`.
- `BUGO_WORKDIR` defaults to the current working directory at startup.
- History context is selected from entries after the latest tape anchor.

## 4. Skills loading

### 4.1 Built-in skills (default)

Repository `skills/` are embedded into the binary via `embed.FS` and loaded by default.
Current built-ins include:

- `skill-creator`
- `skill-installer`

### 4.2 External skills

Bugo loads external skills from:

- `$BUGO_WORKDIR/.agents/skills` (or startup CWD when `BUGO_WORKDIR` is not set)

If an external skill has the same name as a built-in one, the external skill overrides it.

## 5. Telegram behavior

- Private chats are handled by default.
- Group chats are handled only when mention/reply conditions match.
- Session key format: `telegram:<chat_id>`.
- Inputs with `,` prefix go to local command channel.
- Assistant replies are streamed back to Telegram draft messages and finalized as normal messages.

Command examples:

```text
,help
,tools
,tool.describe name=fs.read
,git status
,fs.read path=README.md
,fs.write path=notes/todo.txt content="hello"
,fs.edit path=notes/todo.txt old=hello new=world
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

## 6. Tape storage

Default location:

```text
~/.bugo/tapes/*.jsonl
```

Each session maps to one append-only JSONL tape file.

## 7. Local development

```bash
go mod tidy
go mod vendor
go test ./...
go build -o bugo .
```
