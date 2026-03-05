package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
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
	"web.fetch":     {Name: "web.fetch", Description: "Fetch URL body", Usage: ",web.fetch url=https://example.com"},
	"web.search":    {Name: "web.search", Description: "Build a DuckDuckGo search URL", Usage: ",web.search query=golang"},
	"schedule.add":  {Name: "schedule.add", Description: "Add a cron schedule", Usage: ",schedule.add cron='*/5 * * * *' message='echo hello'"},
	"schedule.list": {Name: "schedule.list", Description: "List scheduled jobs", Usage: ",schedule.list"},
	"schedule.remove": {
		Name:        "schedule.remove",
		Description: "Remove a scheduled job",
		Usage:       ",schedule.remove job_id=my-job",
	},
	"tape.handoff": {Name: "tape.handoff", Description: "Create anchor handoff", Usage: ",tape.handoff name=phase-1 summary='Bootstrap complete'"},
	"tape.anchors": {Name: "tape.anchors", Description: "List tape anchors", Usage: ",tape.anchors"},
	"tape.info":    {Name: "tape.info", Description: "Show tape summary", Usage: ",tape.info"},
	"tape.search":  {Name: "tape.search", Description: "Search tape entries", Usage: ",tape.search query=error"},
	"tape.recent":  {Name: "tape.recent", Description: "Show recent tape entries (compat)", Usage: ",tape.recent limit=10"},
	"tape.reset":   {Name: "tape.reset", Description: "Reset tape", Usage: ",tape.reset archive=true"},
	"skills.list":  {Name: "skills.list", Description: "List discovered skills", Usage: ",skills.list"},
	"quit":         {Name: "quit", Description: "Exit program (CLI semantics)", Usage: ",quit"},
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
	a.enqueuePrompt(inbox, message, true)
}

func (a *App) handleCommand(ctx context.Context, inbox *sessionInbox, content string) {
	result, err := a.executeCommand(ctx, inbox, content)
	if err != nil {
		_ = a.sendText(ctx, inbox.chatID, "Error: "+err.Error())
		return
	}
	if strings.TrimSpace(result) == "" {
		return
	}
	_ = a.sendText(ctx, inbox.chatID, result)
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
	case "fs.read":
		return a.execFSRead(parsed)
	case "fs.write":
		return a.execFSWrite(parsed)
	case "fs.edit":
		return a.execFSEdit(parsed)
	case "web.fetch":
		return a.execWebFetch(ctx, parsed)
	case "web.search":
		return a.execWebSearch(parsed)
	case "quit":
		return "exit", nil
	default:
		return "", fmt.Errorf("unknown command: %s", name)
	}
}

func (a *App) commandHelpText() string {
	return strings.Join([]string{
		"Commands use ',' at line start.",
		"Known names map to internal tools; other commands run through bash.",
		"Examples:",
		"  ,help",
		"  ,git status",
		"  , ls -la",
		"  ,tools",
		"  ,tool.describe name=fs.read",
		"  ,tape.handoff name=phase-1 summary='Bootstrap complete'",
		"  ,tape.anchors",
		"  ,tape.info",
		"  ,tape.search query=error",
		"  ,schedule.add cron='*/5 * * * *' message='echo hello'",
		"  ,schedule.list",
		"  ,schedule.remove job_id=my-job",
		"  ,skills.list",
		"  ,quit",
	}, "\n")
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
	inbox.session.Reset()
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
	if len(a.skills) == 0 {
		return "(no skills)"
	}
	names := make([]string, 0, len(a.skills))
	skillsByName := make(map[string]string, len(a.skills))
	for _, skill := range a.skills {
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

func (a *App) execWebFetch(ctx context.Context, parsed parsedCommandArgs) (string, error) {
	targetURL := strings.TrimSpace(parsed.Kwargs["url"])
	if targetURL == "" && len(parsed.Positional) > 0 {
		targetURL = strings.TrimSpace(parsed.Positional[0])
	}
	if targetURL == "" {
		return "", fmt.Errorf("url is required")
	}
	if _, err := url.ParseRequestURI(targetURL); err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 200_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("status=%d\n%s", resp.StatusCode, string(body)), nil
}

func (a *App) execWebSearch(parsed parsedCommandArgs) (string, error) {
	query := strings.TrimSpace(parsed.Kwargs["query"])
	if query == "" && len(parsed.Positional) > 0 {
		query = strings.TrimSpace(strings.Join(parsed.Positional, " "))
	}
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	return "https://duckduckgo.com/?q=" + url.QueryEscape(query), nil
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
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	if strings.TrimSpace(a.workDir) != "" {
		cmd.Dir = a.workDir
	}
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("%w", err)
		}
		return "", fmt.Errorf("%w\n%s", err, text)
	}
	if text == "" {
		return "(no output)", nil
	}
	return text, nil
}

func (a *App) resolveWorkspacePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path is required")
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
