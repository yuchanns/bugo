package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/go-kratos/blades"
	bladestools "github.com/go-kratos/blades/tools"
)

func addFuncTool[I, O any](
	allTools *[]bladestools.Tool,
	name string,
	description string,
	handler func(context.Context, I) (O, error),
) error {
	tool, err := bladestools.NewFunc(name, description, handler)
	if err != nil {
		return err
	}
	*allTools = append(*allTools, tool)
	return nil
}

type bashToolInput struct {
	Cmd string `json:"cmd"`
}

func (a *App) handleBashTool(ctx context.Context, in bashToolInput) (string, error) {
	command := strings.TrimSpace(in.Cmd)
	if command == "" {
		return "", fmt.Errorf("cmd is required")
	}
	return a.executeShell(ctx, command)
}

type fsReadToolInput struct {
	Path string `json:"path"`
}

func (a *App) handleFSReadTool(_ context.Context, in fsReadToolInput) (string, error) {
	return a.execFSRead(parsedCommandArgs{
		Kwargs: map[string]string{"path": in.Path},
	})
}

type fsWriteToolInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (a *App) handleFSWriteTool(_ context.Context, in fsWriteToolInput) (string, error) {
	return a.execFSWrite(parsedCommandArgs{
		Kwargs: map[string]string{
			"path":    in.Path,
			"content": in.Content,
		},
	})
}

type fsEditToolInput struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

func (a *App) handleFSEditTool(_ context.Context, in fsEditToolInput) (string, error) {
	return a.execFSEdit(parsedCommandArgs{
		Kwargs: map[string]string{
			"path": in.Path,
			"old":  in.Old,
			"new":  in.New,
		},
	})
}

type webFetchToolInput struct {
	URL string `json:"url"`
}

func (a *App) handleWebFetchTool(ctx context.Context, in webFetchToolInput) (string, error) {
	return a.execWebFetch(ctx, parsedCommandArgs{
		Kwargs: map[string]string{
			"url": in.URL,
		},
	})
}

type webSearchToolInput struct {
	Query string `json:"query"`
}

func (a *App) handleWebSearchTool(_ context.Context, in webSearchToolInput) (string, error) {
	return a.execWebSearch(parsedCommandArgs{
		Kwargs: map[string]string{
			"query": in.Query,
		},
	})
}

type scheduleAddToolInput struct {
	Cron    string `json:"cron"`
	Message string `json:"message"`
	JobID   string `json:"job_id,omitempty"`
	ChatID  int64  `json:"chat_id,omitempty"`
}

func (a *App) handleScheduleAddTool(ctx context.Context, in scheduleAddToolInput) (ScheduledJob, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return ScheduledJob{}, err
	}

	chatID := in.ChatID
	if chatID == 0 {
		if s, ok := blades.FromSessionContext(ctx); ok && s != nil {
			chatID = int64FromAny(s.State()["chat_id"])
		}
	}
	if chatID == 0 {
		return ScheduledJob{}, fmt.Errorf("chat_id is required")
	}

	return a.schedule.Add(sessionID, chatID, in.Cron, in.Message, in.JobID)
}

type scheduleListToolInput struct {
	RequestID string `json:"request_id,omitempty"`
}

type scheduleListToolOutput struct {
	Jobs []ScheduledJob `json:"jobs"`
}

func (a *App) handleScheduleListTool(ctx context.Context, _ scheduleListToolInput) (scheduleListToolOutput, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return scheduleListToolOutput{}, err
	}
	return scheduleListToolOutput{
		Jobs: a.schedule.List(sessionID),
	}, nil
}

type scheduleRemoveToolInput struct {
	JobID string `json:"job_id"`
}

type scheduleRemoveToolOutput struct {
	Removed bool `json:"removed"`
}

func (a *App) handleScheduleRemoveTool(ctx context.Context, in scheduleRemoveToolInput) (scheduleRemoveToolOutput, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return scheduleRemoveToolOutput{}, err
	}
	removed, err := a.schedule.Remove(sessionID, in.JobID)
	if err != nil {
		return scheduleRemoveToolOutput{}, err
	}
	return scheduleRemoveToolOutput{Removed: removed}, nil
}

type tapeInfoToolInput struct {
	RequestID string `json:"request_id,omitempty"`
}

func (a *App) handleTapeInfoTool(ctx context.Context, _ tapeInfoToolInput) (TapeInfo, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return TapeInfo{}, err
	}
	return a.tapes.Info(sessionID)
}

type tapeAnchorsToolInput struct {
	Limit int `json:"limit,omitempty"`
}

type tapeAnchorsToolOutput struct {
	Anchors []TapeRecord `json:"anchors"`
}

func (a *App) handleTapeAnchorsTool(ctx context.Context, in tapeAnchorsToolInput) (tapeAnchorsToolOutput, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return tapeAnchorsToolOutput{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	anchors, err := a.tapes.Anchors(sessionID, limit)
	if err != nil {
		return tapeAnchorsToolOutput{}, err
	}
	return tapeAnchorsToolOutput{Anchors: anchors}, nil
}

type tapeResetToolInput struct {
	Archive bool `json:"archive,omitempty"`
}

type tapeResetToolOutput struct {
	Result string `json:"result"`
}

func (a *App) handleTapeResetTool(ctx context.Context, in tapeResetToolInput) (tapeResetToolOutput, error) {
	sessionID, err := sessionIDFromContext(ctx)
	if err != nil {
		return tapeResetToolOutput{}, err
	}
	result, err := a.tapes.Reset(sessionID, in.Archive)
	if err != nil {
		return tapeResetToolOutput{}, err
	}
	if s, ok := blades.FromSessionContext(ctx); ok {
		if ts, ok := s.(*TapeSession); ok {
			ts.Reset()
		}
	}
	a.inboxes.resetSession(sessionID)
	return tapeResetToolOutput{Result: result}, nil
}

type skillsListToolInput struct {
	RequestID string `json:"request_id,omitempty"`
}

type skillItem struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type skillsListToolOutput struct {
	Skills []skillItem `json:"skills"`
}

func (a *App) handleSkillsListTool(_ context.Context, _ skillsListToolInput) (skillsListToolOutput, error) {
	skillList := a.currentSkills()
	names := make([]string, 0, len(skillList))
	byName := make(map[string]skillItem, len(skillList))
	for _, skill := range skillList {
		if skill == nil {
			continue
		}
		name := strings.TrimSpace(skill.Name())
		if name == "" {
			continue
		}
		names = append(names, name)
		byName[name] = skillItem{
			Name:        name,
			Description: strings.TrimSpace(skill.Description()),
		}
	}
	sort.Strings(names)
	items := make([]skillItem, 0, len(names))
	for _, name := range names {
		items = append(items, byName[name])
	}
	return skillsListToolOutput{Skills: items}, nil
}
