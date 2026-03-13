package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	log "github.com/yuchanns/bugo/internal/logging"
)

type commandSpec struct {
	Name        string
	Description string
	Usage       string
}

type parsedCommandArgs struct {
	Kwargs     map[string]string
	Positional []string
}

var builtinCommandSpecs = map[string]commandSpec{
	"help":          {Name: "help", Description: "Show command help", Usage: ",help"},
	"tools":         {Name: "tools", Description: "List available tools", Usage: ",tools"},
	"tool.describe": {Name: "tool.describe", Description: "Show one tool detail", Usage: ",tool.describe name=fs.read"},
	"bash":          {Name: "bash", Description: "Run shell command", Usage: ",git status"},
	"fs.read":       {Name: "fs.read", Description: "Read file content", Usage: ",fs.read path=README.md"},
	"fs.write":      {Name: "fs.write", Description: "Write file content", Usage: ",fs.write path=note.txt content='hello'"},
	"fs.edit":       {Name: "fs.edit", Description: "Replace text in file", Usage: ",fs.edit path=note.txt old='hello' new='world'"},
	"schedule.add":  {Name: "schedule.add", Description: "Add a cron schedule", Usage: ",schedule.add cron='*/5 * * * *' message='echo hello'"},
	"schedule.list": {Name: "schedule.list", Description: "List scheduled jobs", Usage: ",schedule.list"},
	"schedule.remove": {
		Name:        "schedule.remove",
		Description: "Remove a scheduled job",
		Usage:       ",schedule.remove job_id=my-job",
	},
	"tape.handoff":  {Name: "tape.handoff", Description: "Create anchor handoff", Usage: ",tape.handoff name=phase-1 summary='Bootstrap complete'"},
	"tape.anchors":  {Name: "tape.anchors", Description: "List tape anchors", Usage: ",tape.anchors"},
	"tape.info":     {Name: "tape.info", Description: "Show tape summary", Usage: ",tape.info"},
	"tape.search":   {Name: "tape.search", Description: "Search tape entries", Usage: ",tape.search query=error"},
	"tape.recent":   {Name: "tape.recent", Description: "Show recent tape entries (compat)", Usage: ",tape.recent limit=10"},
	"tape.reset":    {Name: "tape.reset", Description: "Reset tape", Usage: ",tape.reset archive=true"},
	"skills.list":   {Name: "skills.list", Description: "List discovered skills", Usage: ",skills.list"},
	"skills.reload": {Name: "skills.reload", Description: "Reload skills and rebuild runner", Usage: ",skills.reload"},
	"quit":          {Name: "quit", Description: "Exit program (CLI semantics)", Usage: ",quit"},
}

var commandAliases = map[string]string{
	"tool":    "tool.describe",
	"tape":    "tape.info",
	"handoff": "tape.handoff",
	"anchors": "tape.anchors",
}

func (a *App) handleScheduledMessage(sessionID string, chatID int64, message string) {
	inbox := a.inboxes.Get(sessionID, chatID)
	inbox.session.SetState("session_id", sessionID)
	inbox.session.SetState("channel", "telegram")
	inbox.session.SetState("chat_id", chatID)
	if strings.HasPrefix(strings.TrimSpace(message), ",") {
		a.handleCommand(context.Background(), inbox, message)
		return
	}
	a.enqueuePrompt(inbox, message, message, true)
}

func (a *App) handleCommand(ctx context.Context, inbox *sessionInbox, content string) {
	log.Info().
		Str("session_id", inbox.session.ID()).
		Str("content", log.PrettifyText(content)).
		Msg("session.message received command")

	result, err := a.executeCommand(ctx, inbox, content)
	threadID := threadIDFromState(inbox.session.State())
	if err != nil {
		log.Error().
			Str("session_id", inbox.session.ID()).
			Err(err).
			Msg("session.command.error")
		_ = a.sendText(ctx, inbox.chatID, threadID, "Error: "+err.Error())
		return
	}
	if strings.TrimSpace(result) == "" {
		return
	}
	log.Info().
		Str("session_id", inbox.session.ID()).
		Str("content", log.PrettifyText(result)).
		Msg("session.run.outbound")
	_ = a.sendText(ctx, inbox.chatID, threadID, result)
}

func (a *App) executeCommand(ctx context.Context, inbox *sessionInbox, content string) (string, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(content), ","))
	if raw == "" {
		return "", fmt.Errorf("empty command, use ,help")
	}
	words, err := splitCommandWords(raw)
	if err != nil {
		return "", err
	}
	if len(words) == 0 {
		return "", fmt.Errorf("empty command, use ,help")
	}

	name := words[0]
	args := words[1:]

	if name == "tape" && len(args) > 0 {
		sub := args[0]
		args = args[1:]
		switch sub {
		case "recent":
			name = "tape.recent"
		case "search":
			name = "tape.search"
		case "info":
			name = "tape.info"
		case "anchors":
			name = "tape.anchors"
		case "handoff":
			name = "tape.handoff"
		case "reset":
			name = "tape.reset"
		default:
			args = append([]string{sub}, args...)
			name = "tape.info"
		}
	}

	if aliased, ok := commandAliases[name]; ok {
		name = aliased
	}

	if _, ok := builtinCommandSpecs[name]; !ok {
		return a.executeShell(ctx, raw)
	}

	parsed := parseCommandArgs(args)
	switch name {
	case "help":
		return a.commandHelpText(), nil
	case "tools":
		return a.listToolsText(), nil
	case "tool.describe":
		return a.describeToolText(parsed), nil
	case "bash":
		return a.execBash(ctx, parsed)
	case "tape.handoff":
		return a.execTapeHandoff(inbox, parsed)
	case "tape.anchors":
		return a.execTapeAnchors(inbox, parsed)
	case "tape.info":
		return a.execTapeInfo(inbox)
	case "tape.search":
		return a.execTapeSearch(inbox, parsed)
	case "tape.recent":
		return a.execTapeRecent(inbox, parsed)
	case "tape.reset":
		return a.execTapeReset(inbox, parsed)
	case "schedule.add":
		return a.execScheduleAdd(inbox, parsed)
	case "schedule.list":
		return a.execScheduleList(inbox), nil
	case "schedule.remove":
		return a.execScheduleRemove(inbox, parsed)
	case "skills.list":
		return a.execSkillsList(), nil
	case "skills.reload":
		return a.execSkillsReload()
	case "fs.read":
		return a.execFSRead(parsed)
	case "fs.write":
		return a.execFSWrite(parsed)
	case "fs.edit":
		return a.execFSEdit(parsed)
	case "quit":
		return "exit", nil
	default:
		return "", fmt.Errorf("unknown command: %s", name)
	}
}

func (a *App) commandHelpText() string {
	names := make([]string, 0, len(builtinCommandSpecs))
	for name := range builtinCommandSpecs {
		names = append(names, name)
	}
	sort.Strings(names)

	lines := []string{
		"Commands use ',' at line start.",
		"Known names map to internal commands; other comma-prefixed lines run through bash.",
		"Available commands:",
	}
	for _, name := range names {
		spec := builtinCommandSpecs[name]
		usage := strings.TrimSpace(spec.Usage)
		if usage == "" {
			usage = "," + name
		}
		lines = append(lines, fmt.Sprintf("  %s  # %s", usage, spec.Description))
	}
	if len(commandAliases) > 0 {
		lines = append(lines, "Aliases:")
		aliasNames := make([]string, 0, len(commandAliases))
		for name := range commandAliases {
			aliasNames = append(aliasNames, name)
		}
		sort.Strings(aliasNames)
		for _, alias := range aliasNames {
			lines = append(lines, fmt.Sprintf("  ,%s -> ,%s", alias, commandAliases[alias]))
		}
	}
	lines = append(lines, "Shell examples:")
	lines = append(lines, "  ,git status")
	lines = append(lines, "  , ls -la")
	return strings.Join(lines, "\n")
}

func (a *App) listToolsText() string {
	names := make([]string, 0, len(builtinCommandSpecs))
	for name := range builtinCommandSpecs {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]string, 0, len(names))
	for _, name := range names {
		spec := builtinCommandSpecs[name]
		rows = append(rows, fmt.Sprintf("%s: %s", spec.Name, spec.Description))
	}
	return strings.Join(rows, "\n")
}

func (a *App) describeToolText(parsed parsedCommandArgs) string {
	name := strings.TrimSpace(parsed.Kwargs["name"])
	if name == "" && len(parsed.Positional) > 0 {
		name = strings.TrimSpace(parsed.Positional[0])
	}
	spec, ok := builtinCommandSpecs[name]
	if !ok {
		return fmt.Sprintf("unknown tool: %s", name)
	}
	return strings.Join([]string{
		fmt.Sprintf("name=%s", spec.Name),
		fmt.Sprintf("description=%s", spec.Description),
		fmt.Sprintf("usage=%s", spec.Usage),
	}, "\n")
}

func (a *App) execTapeHandoff(inbox *sessionInbox, parsed parsedCommandArgs) (string, error) {
	name := strings.TrimSpace(parsed.Kwargs["name"])
	if name == "" && len(parsed.Positional) > 0 {
		name = strings.TrimSpace(parsed.Positional[0])
	}
	if name == "" {
		name = "handoff"
	}
	summary := strings.TrimSpace(parsed.Kwargs["summary"])
	if summary == "" && len(parsed.Positional) > 1 {
		summary = strings.TrimSpace(strings.Join(parsed.Positional[1:], " "))
	}
	nextSteps := strings.TrimSpace(parsed.Kwargs["next_steps"])

	payload := map[string]any{"name": name}
	if summary != "" {
		payload["summary"] = summary
	}
	if nextSteps != "" {
		payload["next_steps"] = nextSteps
	}
	if err := a.tapes.Append(inbox.session.ID(), "anchor", payload); err != nil {
		return "", err
	}
	return fmt.Sprintf("handoff created: %s", name), nil
}

func (a *App) execTapeAnchors(inbox *sessionInbox, parsed parsedCommandArgs) (string, error) {
	limit := intFromKV(parsed.Kwargs, "limit", 50)
	anchors, err := a.tapes.Anchors(inbox.session.ID(), limit)
	if err != nil {
		return "", err
	}
	if len(anchors) == 0 {
		return "(no anchors)", nil
	}
	rows := make([]string, 0, len(anchors))
	for _, rec := range anchors {
		name := strings.TrimSpace(fmt.Sprintf("%v", rec.Payload["name"]))
		if name == "" {
			name = rec.Kind
		}
		stateJSON, _ := json.Marshal(rec.Payload)
		rows = append(rows, fmt.Sprintf("%s state=%s", name, string(stateJSON)))
	}
	return strings.Join(rows, "\n"), nil
}

func (a *App) execTapeInfo(inbox *sessionInbox) (string, error) {
	info, err := a.tapes.Info(inbox.session.ID())
	if err != nil {
		return "", err
	}
	lastAnchor := info.LastAnchor
	if lastAnchor == "" {
		lastAnchor = "-"
	}
	return strings.Join([]string{
		fmt.Sprintf("tape=%s", info.Name),
		fmt.Sprintf("entries=%d", info.Entries),
		fmt.Sprintf("anchors=%d", info.Anchors),
		fmt.Sprintf("last_anchor=%s", lastAnchor),
		fmt.Sprintf("entries_since_last_anchor=%d", info.EntriesSinceLastAnchor),
		"last_token_usage=unknown",
	}, "\n"), nil
}

func (a *App) execTapeSearch(inbox *sessionInbox, parsed parsedCommandArgs) (string, error) {
	query := strings.TrimSpace(parsed.Kwargs["query"])
	if query == "" && len(parsed.Positional) > 0 {
		query = strings.TrimSpace(strings.Join(parsed.Positional, " "))
	}
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := intFromKV(parsed.Kwargs, "limit", 20)
	records, err := a.tapes.Search(inbox.session.ID(), query, limit)
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		return "(no matches)", nil
	}
	rows := make([]string, 0, len(records))
	for _, rec := range records {
		payloadJSON, _ := json.Marshal(rec.Payload)
		rows = append(rows, fmt.Sprintf("#%s %s %s", rec.ID, rec.Kind, string(payloadJSON)))
	}
	return strings.Join(rows, "\n"), nil
}

func (a *App) execTapeRecent(inbox *sessionInbox, parsed parsedCommandArgs) (string, error) {
	limit := intFromKV(parsed.Kwargs, "limit", 20)
	if len(parsed.Positional) > 0 {
		if v, err := strconv.Atoi(parsed.Positional[0]); err == nil && v > 0 {
			limit = v
		}
	}
	records, err := a.tapes.Recent(inbox.session.ID(), limit)
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		return "(no records)", nil
	}
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (a *App) execTapeReset(inbox *sessionInbox, parsed parsedCommandArgs) (string, error) {
	archive := boolFromKV(parsed.Kwargs, "archive")
	result, err := a.tapes.Reset(inbox.session.ID(), archive)
	if err != nil {
		return "", err
	}
	if err := a.tapes.EnsureBootstrapAnchor(inbox.session.ID()); err != nil {
		return "", err
	}
	inbox.session.Reset()
	inbox.resetRuntime()
	return result, nil
}

func (a *App) execScheduleAdd(inbox *sessionInbox, parsed parsedCommandArgs) (string, error) {
	cronSpec := strings.TrimSpace(parsed.Kwargs["cron"])
	message := strings.TrimSpace(parsed.Kwargs["message"])
	if cronSpec == "" && len(parsed.Positional) > 0 {
		cronSpec = strings.TrimSpace(parsed.Positional[0])
	}
	if message == "" && len(parsed.Positional) > 1 {
		message = strings.TrimSpace(strings.Join(parsed.Positional[1:], " "))
	}
	jobID := strings.TrimSpace(parsed.Kwargs["job_id"])

	job, err := a.schedule.Add(inbox.session.ID(), inbox.chatID, cronSpec, message, jobID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("scheduled: job_id=%s cron=%s", job.ID, job.Cron), nil
}

func (a *App) execScheduleList(inbox *sessionInbox) string {
	jobs := a.schedule.List(inbox.session.ID())
	if len(jobs) == 0 {
		return "(no jobs)"
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	rows := make([]string, 0, len(jobs))
	for _, job := range jobs {
		rows = append(rows, fmt.Sprintf(
			"%s cron=%q message=%q created_at=%s",
			job.ID,
			job.Cron,
			job.Message,
			job.CreatedAt.Format(time.RFC3339),
		))
	}
	return strings.Join(rows, "\n")
}

func (a *App) execScheduleRemove(inbox *sessionInbox, parsed parsedCommandArgs) (string, error) {
	jobID := strings.TrimSpace(parsed.Kwargs["job_id"])
	if jobID == "" && len(parsed.Positional) > 0 {
		jobID = strings.TrimSpace(parsed.Positional[0])
	}
	removed, err := a.schedule.Remove(inbox.session.ID(), jobID)
	if err != nil {
		return "", err
	}
	if !removed {
		return "job not found", nil
	}
	return fmt.Sprintf("removed job: %s", jobID), nil
}

func (a *App) execSkillsList() string {
	skillList := a.currentSkills()
	if len(skillList) == 0 {
		return "(no skills)"
	}
	names := make([]string, 0, len(skillList))
	skillsByName := make(map[string]string, len(skillList))
	for _, skill := range skillList {
		if skill == nil {
			continue
		}
		name := strings.TrimSpace(skill.Name())
		if name == "" {
			continue
		}
		names = append(names, name)
		skillsByName[name] = strings.TrimSpace(skill.Description())
	}
	sort.Strings(names)
	rows := make([]string, 0, len(names))
	for _, name := range names {
		rows = append(rows, fmt.Sprintf("%s: %s", name, skillsByName[name]))
	}
	return strings.Join(rows, "\n")
}

func (a *App) execSkillsReload() (string, error) {
	if err := a.reloadAgent(); err != nil {
		return "", err
	}
	return fmt.Sprintf("skills reloaded: %d", len(a.currentSkills())), nil
}

func (a *App) execFSRead(parsed parsedCommandArgs) (string, error) {
	path := strings.TrimSpace(parsed.Kwargs["path"])
	if path == "" && len(parsed.Positional) > 0 {
		path = strings.TrimSpace(parsed.Positional[0])
	}
	resolved, err := a.resolveWorkspacePath(path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (a *App) execFSWrite(parsed parsedCommandArgs) (string, error) {
	path := strings.TrimSpace(parsed.Kwargs["path"])
	content := parsed.Kwargs["content"]
	if path == "" && len(parsed.Positional) > 0 {
		path = strings.TrimSpace(parsed.Positional[0])
	}
	if strings.TrimSpace(content) == "" && len(parsed.Positional) > 1 {
		content = strings.Join(parsed.Positional[1:], " ")
	}
	resolved, err := a.resolveWorkspacePath(path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(resolved, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("written: %s", resolved), nil
}

func (a *App) execFSEdit(parsed parsedCommandArgs) (string, error) {
	path := strings.TrimSpace(parsed.Kwargs["path"])
	oldText := parsed.Kwargs["old"]
	newText := parsed.Kwargs["new"]
	if oldText == "" {
		oldText = parsed.Kwargs["from"]
	}
	if newText == "" {
		newText = parsed.Kwargs["to"]
	}
	if path == "" && len(parsed.Positional) > 0 {
		path = strings.TrimSpace(parsed.Positional[0])
	}
	resolved, err := a.resolveWorkspacePath(path)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	source := string(b)
	if oldText == "" {
		return "", fmt.Errorf("old is required")
	}
	if !strings.Contains(source, oldText) {
		return "", fmt.Errorf("old text not found")
	}
	edited := strings.Replace(source, oldText, newText, 1)
	if err := os.WriteFile(resolved, []byte(edited), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited: %s", resolved), nil
}

func (a *App) execBash(ctx context.Context, parsed parsedCommandArgs) (string, error) {
	command := strings.TrimSpace(parsed.Kwargs["cmd"])
	if command == "" && len(parsed.Positional) > 0 {
		command = strings.TrimSpace(strings.Join(parsed.Positional, " "))
	}
	if command == "" {
		return "", fmt.Errorf("cmd is required")
	}
	return a.executeShell(ctx, command)
}

func (a *App) executeShell(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-c", command)
	cmd.Env = a.minimalShellEnv()
	if strings.TrimSpace(a.workDir) != "" {
		cmd.Dir = a.workDir
	}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return err.Error(), nil
		}
		return err.Error() + "\n" + text, nil
	}
	if text == "" {
		return "(no output)", nil
	}
	return text, nil
}

func (a *App) minimalShellEnv() []string {
	envMap := a.minimalShellEnvMap()
	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}
	return env
}

func (a *App) minimalShellEnvMap() map[string]string {
	keys := append([]string(nil), defaultShellEnvKeys...)
	if len(a.cfg.BashAllowEnv) > 0 {
		keys = append(keys, a.cfg.BashAllowEnv...)
	}
	env := make(map[string]string, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := env[key]; exists {
			continue
		}
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return env
}

var defaultShellEnvKeys = []string{
	"PATH",
	"LANG",
	"LC_ALL",
	"HOME",
	"TMPDIR",
	"TZ",
	"DISPLAY",
	"WAYLAND_DISPLAY",
	"XAUTHORITY",
	"XDG_RUNTIME_DIR",
}

func (a *App) resolveWorkspacePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path is required")
	}
	allowedEnv := a.minimalShellEnvMap()
	raw = os.Expand(raw, func(key string) string {
		return allowedEnv[key]
	})
	if raw == "~" {
		raw = allowedEnv["HOME"]
	} else if strings.HasPrefix(raw, "~/") {
		home := strings.TrimSpace(allowedEnv["HOME"])
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~/"))
	}
	base := strings.TrimSpace(a.workDir)
	if base == "" {
		base, _ = os.Getwd()
	}
	target := raw
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	target = filepath.Clean(target)

	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %s", raw)
	}
	return target, nil
}

func splitCommandWords(input string) ([]string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, nil
	}

	var (
		words    []string
		buf      strings.Builder
		quote    rune
		escaping bool
	)
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		words = append(words, buf.String())
		buf.Reset()
	}

	for _, r := range trimmed {
		switch {
		case escaping:
			buf.WriteRune(r)
			escaping = false
		case r == '\\':
			escaping = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			buf.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case unicode.IsSpace(r):
			flush()
		default:
			buf.WriteRune(r)
		}
	}
	if escaping {
		buf.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in command")
	}
	flush()
	return words, nil
}

func parseCommandArgs(tokens []string) parsedCommandArgs {
	out := parsedCommandArgs{
		Kwargs:     map[string]string{},
		Positional: make([]string, 0, len(tokens)),
	}
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if strings.HasPrefix(token, "--") {
			key := strings.TrimPrefix(token, "--")
			if key == "" {
				continue
			}
			if strings.Contains(key, "=") {
				parts := strings.SplitN(key, "=", 2)
				out.Kwargs[parts[0]] = parts[1]
				continue
			}
			if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "--") {
				out.Kwargs[key] = tokens[i+1]
				i++
				continue
			}
			out.Kwargs[key] = "true"
			continue
		}
		if strings.Contains(token, "=") {
			parts := strings.SplitN(token, "=", 2)
			out.Kwargs[parts[0]] = parts[1]
			continue
		}
		out.Positional = append(out.Positional, token)
	}
	return out
}

func intFromKV(kwargs map[string]string, key string, defaultValue int) int {
	raw := strings.TrimSpace(kwargs[key])
	if raw == "" {
		return defaultValue
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultValue
	}
	return v
}

func boolFromKV(kwargs map[string]string, key string) bool {
	raw := strings.TrimSpace(kwargs[key])
	if raw == "" {
		return false
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return v
}
