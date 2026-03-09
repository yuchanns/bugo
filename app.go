package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/blades"
	"github.com/go-kratos/blades/contrib/openai"
	"github.com/go-kratos/blades/skills"
	"github.com/go-kratos/blades/tools"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/openai/openai-go/v3/option"
)

type App struct {
	cfg      Config
	tapes    *TapeStore
	sessions *SessionStore
	inboxes  *inboxHub
	runner   *blades.Runner
	bot      *bot.Bot
	botUser  *models.User
	skills   []skills.Skill
	schedule *ScheduleStore
	workDir  string
	agentMu  sync.RWMutex
}

func NewApp(cfg Config) (*App, error) {
	if err := os.MkdirAll(cfg.HomeDir, 0o755); err != nil {
		return nil, err
	}
	workDir, err := resolveWorkDir(cfg.WorkDir)
	if err != nil {
		return nil, err
	}

	tapes, err := NewTapeStore(filepath.Join(cfg.HomeDir, "tapes"), cfg.Model)
	if err != nil {
		return nil, err
	}
	sessions := NewSessionStore(tapes)

	app := &App{
		cfg:      cfg,
		tapes:    tapes,
		sessions: sessions,
		inboxes:  newInboxHub(sessions),
		workDir:  workDir,
	}
	app.schedule, err = NewScheduleStore(func(sessionID string, chatID int64, message string) {
		app.handleScheduledMessage(sessionID, chatID, message)
	})
	if err != nil {
		return nil, err
	}

	if err := app.reloadAgent(); err != nil {
		return nil, err
	}
	return app, nil
}

func resolveWorkDir(raw string) (string, error) {
	base, err := os.Getwd()
	if err != nil {
		return "", err
	}
	target := strings.TrimSpace(raw)
	if target == "" {
		return base, nil
	}
	target = resolveHomeDir(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(base, target)
	}
	target = filepath.Clean(target)
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workdir is not a directory: %s", target)
	}
	return target, nil
}

func resolveExternalSkillsDir(workDir string) (string, error) {
	base := strings.TrimSpace(workDir)
	if base == "" {
		var err error
		base, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	return filepath.Clean(filepath.Join(base, ".agents", "skills")), nil
}

func (a *App) buildAgent() (blades.Agent, error) {
	modelConfig := openai.Config{
		BaseURL:         a.cfg.APIBase,
		APIKey:          a.cfg.APIKey,
		MaxOutputTokens: int64(a.cfg.MaxOutputTokens),
		RequestOptions: []option.RequestOption{
			option.WithHTTPClient(newOpenAIHTTPClient()),
		},
	}
	model := openai.NewModel(a.cfg.Model, modelConfig)

	baseTools, err := a.buildTools()
	if err != nil {
		return nil, err
	}

	skillList, err := a.loadSkills()
	if err != nil {
		return nil, err
	}
	tools := baseTools
	instruction := a.systemInstruction()

	return blades.NewAgent(
		"bugo",
		blades.WithModel(model),
		blades.WithInstruction(instruction),
		blades.WithTools(tools...),
		blades.WithSkills(skillList...),
		blades.WithMiddleware(
			tapeContextMiddleware(a.tapes),
			workspaceAgentsPromptMiddleware(a.workDir),
			patchToolSchemas(),
		),
		blades.WithMaxIterations(a.cfg.ModelMaxIterations),
	)
}

func newOpenAIHTTPClient() *http.Client {
	return &http.Client{
		Transport: &openAIStatusLoggingTransport{
			base: http.DefaultTransport,
		},
	}
}

type openAIStatusLoggingTransport struct {
	base http.RoundTripper
}

func (t *openAIStatusLoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		return resp, nil
	}
	resp.Body = &openAIResponseBodyLogger{
		body:      resp.Body,
		method:    req.Method,
		url:       req.URL.String(),
		status:    resp.StatusCode,
		shouldLog: resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices,
	}
	return resp, nil
}

type openAIResponseBodyLogger struct {
	body      io.ReadCloser
	method    string
	url       string
	status    int
	shouldLog bool
}

func (r *openAIResponseBodyLogger) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if r.shouldLog && n > 0 {
		log.Printf("openai non-2xx response chunk: method=%s url=%s status=%d chunk=%s", r.method, r.url, r.status, string(p[:n]))
	}
	return n, err
}

func (r *openAIResponseBodyLogger) Close() error {
	return r.body.Close()
}

func (a *App) loadSkills() ([]skills.Skill, error) {
	builtin, err := skills.NewFromEmbed(builtinSkillsFS)
	if err != nil {
		return nil, err
	}

	var external []skills.Skill
	externalDir, err := resolveExternalSkillsDir(a.workDir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(externalDir)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if !info.IsDir() {
			return nil, fmt.Errorf("extra skills path is not a directory: %s", externalDir)
		}
		external, err = skills.NewFromDir(externalDir)
		if err != nil {
			return nil, err
		}
	}

	merged := mergeSkills(builtin, external)
	a.agentMu.Lock()
	a.skills = merged
	a.agentMu.Unlock()
	log.Printf("loaded skills: builtin=%d external=%d merged=%d", len(builtin), len(external), len(merged))
	return merged, nil
}

func (a *App) reloadAgent() error {
	agent, err := a.buildAgent()
	if err != nil {
		return err
	}
	runner := blades.NewRunner(agent, blades.WithResumable(true))
	a.agentMu.Lock()
	a.runner = runner
	a.agentMu.Unlock()
	return nil
}

func (a *App) currentRunner() *blades.Runner {
	a.agentMu.RLock()
	defer a.agentMu.RUnlock()
	return a.runner
}

func (a *App) currentSkills() []skills.Skill {
	a.agentMu.RLock()
	defer a.agentMu.RUnlock()
	if len(a.skills) == 0 {
		return nil
	}
	out := make([]skills.Skill, len(a.skills))
	copy(out, a.skills)
	return out
}

func mergeSkills(base []skills.Skill, extra []skills.Skill) []skills.Skill {
	merged := make(map[string]skills.Skill, len(base)+len(extra))
	for _, s := range base {
		if s == nil {
			continue
		}
		merged[s.Name()] = s
	}
	for _, s := range extra {
		if s == nil {
			continue
		}
		// External skills override embedded skills with same name.
		merged[s.Name()] = s
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]skills.Skill, 0, len(names))
	for _, name := range names {
		out = append(out, merged[name])
	}
	return out
}

func (a *App) buildTools() ([]tools.Tool, error) {
	allTools := make([]tools.Tool, 0, 24)

	if err := addFuncTool(&allTools, "telegram_send", "Send a message to Telegram. If chat_id is omitted, use current session chat.", a.handleTelegramSendTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "telegram_edit", "Edit a Telegram message text. Prefer editing the previous message_id to update progress.", a.handleTelegramEditTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "telegram_get_update", "Wait for next user update in current Telegram session after asking a question.", a.handleTelegramGetUpdateTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "tape_recent", "Get latest tape records from current session.", a.handleTapeRecentTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "tape_search", "Search tape records in current session.", a.handleTapeSearchTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "tape_handoff", "Write a compact handoff summary into tape.", a.handleTapeHandoffTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "bash", "Run shell command in workspace.", a.handleBashTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "fs_read", "Read file content from workspace.", a.handleFSReadTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "fs_write", "Write file content into workspace.", a.handleFSWriteTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "fs_edit", "Replace first matched text in a file.", a.handleFSEditTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "schedule_add", "Add a cron schedule for current session chat.", a.handleScheduleAddTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "schedule_list", "List schedules for current session.", a.handleScheduleListTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "schedule_remove", "Remove one schedule by job_id in current session.", a.handleScheduleRemoveTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "tape_info", "Show tape summary for current session.", a.handleTapeInfoTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "tape_anchors", "List tape anchors for current session.", a.handleTapeAnchorsTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "tape_reset", "Reset current session tape, optionally with archive.", a.handleTapeResetTool); err != nil {
		return nil, err
	}
	if err := addFuncTool(&allTools, "skills_list", "List loaded skills.", a.handleSkillsListTool); err != nil {
		return nil, err
	}

	return allTools, nil
}

func (a *App) systemInstruction() string {
	return strings.TrimSpace(`
<runtime_contract>
1. Use tool calls for all actions (file ops, shell, tape, skills, scheduling).
2. Do not emit comma-prefixed commands in normal flow; use tool calls instead.
3. If a compatibility fallback is required, runtime can still parse comma commands.
4. Never emit '<command ...>' blocks yourself; those are runtime-generated.
5. When enough evidence is collected, return plain natural language answer.
6. Use '$name' hints to request detail expansion for tools/skills when needed.
7. The "bash" tool is available in this runtime; do not claim shell access is unavailable when bash can be used.
8. If the user asks for runtime/system information, first call "bash" with safe read-only commands (for example: uname -a, cat /etc/os-release) and then summarize outputs.
</runtime_contract>
<context_contract>
Excessively long context may cause model call failures. In this case, you SHOULD first use tape_handoff tool to shorten the retrieved history.
</context_contract>
<response_instruct>
You are handling Telegram messages.
If proactive response mode is enabled, you MUST call "telegram_send" or "telegram_edit" before finishing.
When sending progress updates, prefer "telegram_edit" on the same message_id after an initial "telegram_send".
After asking the user a question via "telegram_send"/"telegram_edit", you can call "telegram_get_update" to wait for reply.
If proactive response mode is disabled, runtime can auto-send your assistant text.
Session metadata is in state keys like "channel", "chat_id", "session_id".
</response_instruct>
`)
}

func (a *App) Run(ctx context.Context) error {
	httpClient, err := a.buildHTTPClient()
	if err != nil {
		return err
	}

	opts := []bot.Option{
		bot.WithDefaultHandler(a.onUpdate),
		bot.WithAllowedUpdates(bot.AllowedUpdates{models.AllowedUpdateMessage}),
		bot.WithWorkers(a.cfg.TelegramWorkers),
		bot.WithSkipGetMe(),
	}
	if httpClient != nil {
		opts = append(opts, bot.WithHTTPClient(time.Minute, httpClient))
	}

	botClient, err := bot.New(a.cfg.TelegramToken, opts...)
	if err != nil {
		return err
	}
	a.bot = botClient
	a.botUser, _ = botClient.GetMe(ctx)
	if a.botUser != nil {
		log.Printf("telegram bot ready: id=%d username=%s", a.botUser.ID, a.botUser.Username)
	}
	log.Printf("bugo started: model=%s proactive=%v", a.cfg.Model, a.cfg.ProactiveResponse)
	if a.schedule != nil {
		defer a.schedule.Close()
	}
	botClient.Start(ctx)
	return nil
}

func (a *App) buildHTTPClient() (*http.Client, error) {
	if strings.TrimSpace(a.cfg.TelegramProxy) == "" {
		return nil, nil
	}
	u, err := url.Parse(a.cfg.TelegramProxy)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy: http.ProxyURL(u),
	}
	return &http.Client{
		Timeout:   time.Minute,
		Transport: tr,
	}, nil
}

func (a *App) onUpdate(ctx context.Context, _ *bot.Bot, update *models.Update) {
	if update == nil || update.Message == nil {
		return
	}
	msg := update.Message
	content, media := parseMessageContent(msg)
	if !a.isChatAllowed(msg.Chat.ID) {
		return
	}
	if !a.isSenderAllowed(msg.From) {
		_, _ = a.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Access denied.",
		})
		return
	}

	if content == "" {
		return
	}
	if content == "/start" {
		_, _ = a.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Bugo is online. Send text to start.",
		})
		return
	}
	if after, ok := strings.CutPrefix(content, "/bugo "); ok {
		content = strings.TrimSpace(after)
	}
	if content == "" {
		return
	}

	sessionID := fmt.Sprintf("telegram:%d", msg.Chat.ID)
	inbox := a.inboxes.Get(sessionID, msg.Chat.ID)
	inbox.session.SetState("session_id", sessionID)
	inbox.session.SetState("channel", "telegram")
	inbox.session.SetState("chat_id", msg.Chat.ID)
	inbox.session.SetState("round_telegram_message_id", 0)
	if msg.From != nil {
		inbox.session.SetState("sender_id", msg.From.ID)
		if msg.From.Username != "" {
			inbox.session.SetState("sender_username", msg.From.Username)
		}
	}
	isCommand := strings.HasPrefix(strings.TrimSpace(content), ",")
	inbox.mu.Lock()
	running := inbox.running
	inbox.mu.Unlock()
	if running && !isCommand && a.inboxes.hasWaiter(sessionID) {
		a.inboxes.pushUpdate(sessionID, telegramSessionUpdate{
			MessageID: msg.ID,
			ChatID:    msg.Chat.ID,
			Text:      content,
			Type:      messageType(msg),
			SenderID:  int64FromAny(senderID(msg)),
			Username:  senderUsername(msg),
			FullName:  senderFullName(msg),
			Date:      msg.Date,
		})
		return
	}

	if isCommand {
		a.handleCommand(ctx, inbox, content)
		return
	}

	mentioned := a.isMentioned(msg, content)
	prompt := a.buildPromptPayload(msg, content, media)
	a.enqueuePrompt(inbox, prompt, mentioned)
}

func (a *App) buildPromptPayload(msg *models.Message, content string, media map[string]any) string {
	meta := map[string]any{
		"message":       content,
		"chat_id":       strconv.FormatInt(msg.Chat.ID, 10),
		"message_id":    msg.ID,
		"type":          messageType(msg),
		"sender_id":     senderID(msg),
		"sender_is_bot": senderIsBot(msg),
		"username":      senderUsername(msg),
		"full_name":     senderFullName(msg),
		"date":          msg.Date,
	}
	if len(media) > 0 {
		meta["media"] = media
	}
	if msg.ReplyToMessage != nil {
		meta["reply_to_message"] = map[string]any{
			"message_id": msg.ReplyToMessage.ID,
			"sender_id":  senderID(msg.ReplyToMessage),
			"text":       msg.ReplyToMessage.Text,
		}
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return content
	}
	return string(b)
}

func (a *App) isMentioned(msg *models.Message, content string) bool {
	if msg == nil {
		return false
	}
	switch msg.Chat.Type {
	case models.ChatTypePrivate:
		return true
	case models.ChatTypeGroup, models.ChatTypeSupergroup:
		if msg.ReplyToMessage != nil && msg.ReplyToMessage.From != nil && a.botUser != nil {
			if msg.ReplyToMessage.From.ID == a.botUser.ID {
				return true
			}
		}
		lower := strings.ToLower(content)
		if strings.Contains(lower, "bugo") {
			return true
		}
		if a.botUser != nil && a.botUser.Username != "" {
			if strings.Contains(lower, "@"+strings.ToLower(a.botUser.Username)) {
				return true
			}
		}
		for _, e := range append(msg.Entities, msg.CaptionEntities...) {
			if e.Type == models.MessageEntityTypeTextMention && e.User != nil && a.botUser != nil && e.User.ID == a.botUser.ID {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func (a *App) enqueuePrompt(inbox *sessionInbox, prompt string, mentioned bool) {
	now := time.Now()
	inbox.mu.Lock()
	defer inbox.mu.Unlock()

	if !mentioned {
		if inbox.lastMention.IsZero() || now.Sub(inbox.lastMention) > time.Duration(a.cfg.ActiveWindow)*time.Second {
			return
		}
	}

	inbox.pending = append(inbox.pending, prompt)
	timeout := time.Duration(a.cfg.MessageDelay) * time.Second
	if mentioned {
		inbox.lastMention = now
		timeout = time.Duration(a.cfg.DebounceSeconds) * time.Second
	}

	if inbox.timer != nil {
		inbox.timer.Stop()
	}
	inbox.timer = time.AfterFunc(timeout, func() {
		a.flushInbox(inbox)
	})
}

func (a *App) flushInbox(inbox *sessionInbox) {
	inbox.mu.Lock()
	if inbox.running {
		inbox.mu.Unlock()
		return
	}
	if len(inbox.pending) == 0 {
		inbox.mu.Unlock()
		return
	}
	prompt := strings.Join(inbox.pending, "\n")
	chatID := inbox.chatID
	session := inbox.session
	session.SetState("round_telegram_message_id", 0)
	inbox.pending = nil
	inbox.running = true
	inbox.mu.Unlock()

	cancelTyping := a.startTyping(chatID)
	response, err := a.runModelPrompt(session, prompt)
	cancelTyping()

	if err != nil {
		log.Printf("run prompt error session=%s err=%v", session.ID(), err)
		if sendErr := a.sendText(context.Background(), chatID, "Error: "+err.Error()); sendErr != nil {
			log.Printf("send error message failed session=%s chat_id=%d err=%v", session.ID(), chatID, sendErr)
		}
	} else if !a.cfg.ProactiveResponse && strings.TrimSpace(response) != "" {
		if sendErr := a.sendText(context.Background(), chatID, response); sendErr != nil {
			log.Printf("send assistant response failed session=%s chat_id=%d err=%v", session.ID(), chatID, sendErr)
		}
	}

	inbox.mu.Lock()
	inbox.running = false
	if len(inbox.pending) > 0 {
		if inbox.timer != nil {
			inbox.timer.Stop()
		}
		inbox.timer = time.AfterFunc(time.Duration(a.cfg.MessageDelay)*time.Second, func() {
			a.flushInbox(inbox)
		})
	}
	inbox.mu.Unlock()
}

func (a *App) runModelPrompt(session *TapeSession, prompt string) (string, error) {
	ctx := context.Background()
	runner := a.currentRunner()
	if runner == nil {
		return "", fmt.Errorf("runner is not initialized")
	}

	var (
		deltaBuilder strings.Builder
		finalText    string
	)

	for out, err := range runner.RunStream(ctx, blades.UserMessage(prompt), blades.WithSession(session)) {
		if err != nil {
			return "", err
		}
		if out == nil {
			continue
		}
		if out.Role != blades.RoleAssistant {
			continue
		}

		text := out.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}

		switch out.Status {
		case blades.StatusCompleted:
			finalText = strings.TrimSpace(text)
		case blades.StatusIncomplete, blades.StatusInProgress:
			deltaBuilder.WriteString(text)
		}
	}
	if finalText != "" {
		return finalText, nil
	}
	streamText := strings.TrimSpace(deltaBuilder.String())
	if streamText != "" {
		return streamText, nil
	}
	return "", nil
}

func (a *App) startTyping(chatID int64) func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			_, _ = a.bot.SendChatAction(ctx, &bot.SendChatActionParams{
				ChatID: chatID,
				Action: models.ChatActionTyping,
			})
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return cancel
}

func (a *App) sendText(ctx context.Context, chatID int64, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	parts := splitTextByRunes(text, 3500)
	for _, part := range parts {
		if _, err := a.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   part,
		}); err != nil {
			return err
		}
	}
	return nil
}

func splitTextByRunes(text string, limit int) []string {
	if limit <= 0 {
		return []string{text}
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}
	chunks := make([]string, 0, len(runes)/limit+1)
	for len(runes) > 0 {
		n := min(limit, len(runes))
		chunks = append(chunks, string(runes[:n]))
		runes = runes[n:]
	}
	return chunks
}

func (a *App) isChatAllowed(chatID int64) bool {
	if len(a.cfg.AllowChats) == 0 {
		return true
	}
	_, ok := a.cfg.AllowChats[chatID]
	return ok
}

func (a *App) isSenderAllowed(user *models.User) bool {
	if len(a.cfg.AllowFrom) == 0 {
		return true
	}
	if user == nil {
		return false
	}
	id := strings.ToLower(strconv.FormatInt(user.ID, 10))
	if _, ok := a.cfg.AllowFrom[id]; ok {
		return true
	}
	if user.Username != "" {
		_, ok := a.cfg.AllowFrom[strings.ToLower(user.Username)]
		return ok
	}
	return false
}

type telegramSendInput struct {
	ChatID int64  `json:"chat_id,omitempty"`
	Text   string `json:"text"`
}

type telegramSendOutput struct {
	Sent      bool `json:"sent"`
	MessageID int  `json:"message_id,omitempty"`
}

func (a *App) handleTelegramSendTool(ctx context.Context, in telegramSendInput) (telegramSendOutput, error) {
	chatID := in.ChatID
	if chatID == 0 {
		s, ok := blades.FromSessionContext(ctx)
		if ok {
			chatID = int64FromAny(s.State()["chat_id"])
		}
	}
	if chatID == 0 {
		return telegramSendOutput{}, fmt.Errorf("chat_id is required")
	}
	if strings.TrimSpace(in.Text) == "" {
		return telegramSendOutput{}, fmt.Errorf("text is required")
	}
	resp, err := a.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: chatID,
		Text:   in.Text,
	})
	if err != nil {
		return telegramSendOutput{}, err
	}
	if s, ok := blades.FromSessionContext(ctx); ok && s != nil {
		s.SetState("round_telegram_message_id", resp.ID)
		s.SetState("last_telegram_message_id", resp.ID)
		s.SetState("last_telegram_chat_id", chatID)
	}
	return telegramSendOutput{Sent: true, MessageID: resp.ID}, nil
}

type telegramEditInput struct {
	ChatID    int64  `json:"chat_id,omitempty"`
	MessageID int    `json:"message_id,omitempty"`
	Text      string `json:"text"`
}

type telegramEditOutput struct {
	Edited    bool `json:"edited"`
	MessageID int  `json:"message_id,omitempty"`
}

func (a *App) handleTelegramEditTool(ctx context.Context, in telegramEditInput) (telegramEditOutput, error) {
	chatID := in.ChatID
	messageID := in.MessageID
	if s, ok := blades.FromSessionContext(ctx); ok && s != nil {
		state := s.State()
		if chatID == 0 {
			chatID = int64FromAny(state["chat_id"])
		}
		if messageID == 0 {
			messageID = int(int64FromAny(state["round_telegram_message_id"]))
		}
	}
	if chatID == 0 {
		return telegramEditOutput{}, fmt.Errorf("chat_id is required")
	}
	if messageID <= 0 {
		return telegramEditOutput{}, fmt.Errorf("message_id is required")
	}
	if strings.TrimSpace(in.Text) == "" {
		return telegramEditOutput{}, fmt.Errorf("text is required")
	}
	resp, err := a.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      in.Text,
	})
	if err != nil {
		return telegramEditOutput{}, err
	}
	if s, ok := blades.FromSessionContext(ctx); ok && s != nil {
		s.SetState("round_telegram_message_id", messageID)
		s.SetState("last_telegram_message_id", messageID)
		s.SetState("last_telegram_chat_id", chatID)
	}
	outID := messageID
	if resp != nil && resp.ID != 0 {
		outID = resp.ID
	}
	return telegramEditOutput{Edited: true, MessageID: outID}, nil
}

type telegramGetUpdateInput struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

type telegramGetUpdateItem struct {
	MessageID int    `json:"message_id"`
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	Type      string `json:"type"`
	SenderID  int64  `json:"sender_id"`
	Username  string `json:"username,omitempty"`
	FullName  string `json:"full_name,omitempty"`
	Date      int    `json:"date"`
}

type telegramGetUpdateOutput struct {
	Received bool                   `json:"received"`
	TimedOut bool                   `json:"timed_out,omitempty"`
	Update   *telegramGetUpdateItem `json:"update,omitempty"`
}

func (a *App) handleTelegramGetUpdateTool(ctx context.Context, in telegramGetUpdateInput) (telegramGetUpdateOutput, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return telegramGetUpdateOutput{}, err
	}
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Minute
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	update, ok := a.inboxes.waitForUpdate(sessionID, timeout)
	if !ok {
		return telegramGetUpdateOutput{TimedOut: true}, nil
	}
	return telegramGetUpdateOutput{
		Received: true,
		Update: &telegramGetUpdateItem{
			MessageID: update.MessageID,
			ChatID:    update.ChatID,
			Text:      update.Text,
			Type:      update.Type,
			SenderID:  update.SenderID,
			Username:  update.Username,
			FullName:  update.FullName,
			Date:      update.Date,
		},
	}, nil
}

type tapeRecentInput struct {
	Limit int `json:"limit,omitempty"`
}

type tapeRecentOutput struct {
	Records []TapeRecord `json:"records"`
}

func (a *App) handleTapeRecentTool(ctx context.Context, in tapeRecentInput) (tapeRecentOutput, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return tapeRecentOutput{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	records, err := a.tapes.Recent(sessionID, limit)
	if err != nil {
		return tapeRecentOutput{}, err
	}
	return tapeRecentOutput{Records: records}, nil
}

type tapeSearchInput struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

type tapeSearchOutput struct {
	Records []TapeRecord `json:"records"`
}

func (a *App) handleTapeSearchTool(ctx context.Context, in tapeSearchInput) (tapeSearchOutput, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return tapeSearchOutput{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	records, err := a.tapes.Search(sessionID, in.Query, limit)
	if err != nil {
		return tapeSearchOutput{}, err
	}
	return tapeSearchOutput{Records: records}, nil
}

type tapeHandoffInput struct {
	Summary string `json:"summary"`
}

type tapeHandoffOutput struct {
	Stored bool `json:"stored"`
}

func (a *App) handleTapeHandoffTool(ctx context.Context, in tapeHandoffInput) (tapeHandoffOutput, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return tapeHandoffOutput{}, err
	}
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return tapeHandoffOutput{}, fmt.Errorf("summary is required")
	}
	if err := a.tapes.Append(sessionID, "handoff", map[string]any{"summary": summary}); err != nil {
		return tapeHandoffOutput{}, err
	}
	return tapeHandoffOutput{Stored: true}, nil
}

func sessionIDFromContext(ctx context.Context) (string, error) {
	session, ok := blades.FromSessionContext(ctx)
	if !ok || session == nil {
		return "", fmt.Errorf("session context is missing")
	}
	return session.ID(), nil
}

func int64FromAny(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		i, _ := x.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return i
	default:
		return 0
	}
}

func parseMessageContent(msg *models.Message) (string, map[string]any) {
	if msg == nil {
		return "", nil
	}
	switch {
	case msg.Text != "":
		return msg.Text, nil
	case len(msg.Photo) > 0:
		photo := msg.Photo[len(msg.Photo)-1]
		text := "[Photo message]"
		if msg.Caption != "" {
			text = text + " Caption: " + msg.Caption
		}
		return text, map[string]any{
			"file_id": photo.FileID,
			"width":   photo.Width,
			"height":  photo.Height,
		}
	case msg.Voice != nil:
		return fmt.Sprintf("[Voice message: %ds]", msg.Voice.Duration), map[string]any{"file_id": msg.Voice.FileID}
	case msg.Video != nil:
		return fmt.Sprintf("[Video: %ds]", msg.Video.Duration), map[string]any{"file_id": msg.Video.FileID}
	case msg.Document != nil:
		return "[Document message]", map[string]any{"file_id": msg.Document.FileID, "name": msg.Document.FileName}
	case msg.Audio != nil:
		return "[Audio message]", map[string]any{"file_id": msg.Audio.FileID, "title": msg.Audio.Title}
	case msg.Sticker != nil:
		return "[Sticker message]", map[string]any{"file_id": msg.Sticker.FileID, "emoji": msg.Sticker.Emoji}
	default:
		return "", nil
	}
}

func messageType(msg *models.Message) string {
	if msg == nil {
		return "unknown"
	}
	switch {
	case msg.Text != "":
		return "text"
	case len(msg.Photo) > 0:
		return "photo"
	case msg.Voice != nil:
		return "voice"
	case msg.Video != nil:
		return "video"
	case msg.Document != nil:
		return "document"
	case msg.Audio != nil:
		return "audio"
	case msg.Sticker != nil:
		return "sticker"
	default:
		return "unknown"
	}
}

func senderID(msg *models.Message) string {
	if msg == nil || msg.From == nil {
		return ""
	}
	return strconv.FormatInt(msg.From.ID, 10)
}

func senderUsername(msg *models.Message) string {
	if msg == nil || msg.From == nil {
		return ""
	}
	return msg.From.Username
}

func senderFullName(msg *models.Message) string {
	if msg == nil || msg.From == nil {
		return ""
	}
	return strings.TrimSpace(msg.From.FirstName + " " + msg.From.LastName)
}

func senderIsBot(msg *models.Message) bool {
	if msg == nil || msg.From == nil {
		return false
	}
	return msg.From.IsBot
}
