package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/blades"
	"github.com/google/uuid"
)

type TapeRecord struct {
	ID        string         `json:"id"`
	SessionID string         `json:"session_id"`
	Time      time.Time      `json:"time"`
	Kind      string         `json:"kind"`
	Payload   map[string]any `json:"payload"`
}

type TapeInfo struct {
	Name                   string `json:"name"`
	Entries                int    `json:"entries"`
	Anchors                int    `json:"anchors"`
	LastAnchor             string `json:"last_anchor,omitempty"`
	EntriesSinceLastAnchor int    `json:"entries_since_last_anchor"`
}

type TapeStore struct {
	root  string
	mu    sync.Mutex
	files map[string]*tapeFile
}

var errTapeAnchorNotFound = errors.New("tape anchor not found")

type tapeFile struct {
	records []TapeRecord
	offset  int64
}

func (f *tapeFile) reset() {
	f.records = nil
	f.offset = 0
}

func NewTapeStore(root, _ string) (*TapeStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &TapeStore{
		root:  root,
		files: map[string]*tapeFile{},
	}, nil
}

func (s *TapeStore) Append(sessionID, kind string, payload map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record := TapeRecord{
		ID:        uuid.NewString(),
		SessionID: sessionID,
		Time:      time.Now().UTC(),
		Kind:      kind,
		Payload:   payload,
	}
	return s.appendLocked(record)
}

func (s *TapeStore) AppendMessage(sessionID string, message *blades.Message) error {
	if message == nil {
		return nil
	}
	toolParts := extractToolParts(message.Parts)
	if message.Role == blades.RoleTool && len(toolParts) > 0 {
		callPayload := map[string]any{
			"message_id":    message.ID,
			"invocation_id": message.InvocationID,
			"author":        message.Author,
			"status":        string(message.Status),
			"finish_reason": message.FinishReason,
			"calls":         encodeToolCalls(toolParts),
		}
		if err := s.Append(sessionID, "tool_call", callPayload); err != nil {
			return err
		}
		resultPayload := map[string]any{
			"message_id":    message.ID,
			"invocation_id": message.InvocationID,
			"author":        message.Author,
			"status":        string(message.Status),
			"finish_reason": message.FinishReason,
			"results":       encodeToolResults(toolParts),
		}
		return s.Append(sessionID, "tool_result", resultPayload)
	}
	payload := map[string]any{
		"message_id":    message.ID,
		"invocation_id": message.InvocationID,
		"role":          string(message.Role),
		"author":        message.Author,
		"status":        string(message.Status),
		"finish_reason": message.FinishReason,
		"text":          message.Text(),
		"parts":         encodeMessageParts(message.Parts),
	}
	return s.Append(sessionID, "message", payload)
}

func (s *TapeStore) HistoryMessages(sessionID string) ([]*blades.Message, error) {
	records, err := s.readAll(sessionID)
	if err != nil {
		return nil, err
	}
	selected, err := selectRecordsAfterLastAnchor(records)
	if err != nil {
		return nil, err
	}
	return selectTapeMessages(selected), nil
}

func (s *TapeStore) EnsureBootstrapAnchor(sessionID string) error {
	records, err := s.readAll(sessionID)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if isAnchorRecord(rec) {
			return nil
		}
	}
	return s.Append(sessionID, "anchor", map[string]any{
		"name": "session/start",
		"state": map[string]any{
			"owner": "human",
		},
	})
}

func (s *TapeStore) appendLocked(record TapeRecord) error {
	path := s.filePath(record.SessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	return enc.Encode(record)
}

func (s *TapeStore) Recent(sessionID string, limit int) ([]TapeRecord, error) {
	records, err := s.readAll(sessionID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit >= len(records) {
		return records, nil
	}
	return records[len(records)-limit:], nil
}

func (s *TapeStore) Search(sessionID, query string, limit int) ([]TapeRecord, error) {
	records, err := s.readAll(sessionID)
	if err != nil {
		return nil, err
	}

	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		if limit <= 0 || limit >= len(records) {
			return records, nil
		}
		return records[len(records)-limit:], nil
	}

	matches := make([]TapeRecord, 0, min(limit, len(records)))
	for i := len(records) - 1; i >= 0; i-- {
		b, _ := json.Marshal(records[i].Payload)
		haystack := strings.ToLower(records[i].Kind + "\n" + string(b))
		if strings.Contains(haystack, query) {
			matches = append(matches, records[i])
			if limit > 0 && len(matches) >= limit {
				break
			}
		}
	}

	// Keep chronological order in result.
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}
	return matches, nil
}

func (s *TapeStore) Anchors(sessionID string, limit int) ([]TapeRecord, error) {
	records, err := s.readAll(sessionID)
	if err != nil {
		return nil, err
	}
	anchors := make([]TapeRecord, 0, len(records))
	for _, rec := range records {
		if isAnchorRecord(rec) {
			anchors = append(anchors, rec)
		}
	}
	if limit <= 0 || limit >= len(anchors) {
		return anchors, nil
	}
	return anchors[len(anchors)-limit:], nil
}

func (s *TapeStore) Info(sessionID string) (TapeInfo, error) {
	records, err := s.readAll(sessionID)
	if err != nil {
		return TapeInfo{}, err
	}
	info := TapeInfo{
		Name:    sessionID,
		Entries: len(records),
	}
	lastAnchorIdx := -1
	for idx, rec := range records {
		if !isAnchorRecord(rec) {
			continue
		}
		info.Anchors++
		lastAnchorIdx = idx
		if name := strings.TrimSpace(fmt.Sprintf("%v", rec.Payload["name"])); name != "" {
			info.LastAnchor = name
		}
	}
	if lastAnchorIdx >= 0 {
		info.EntriesSinceLastAnchor = len(records) - lastAnchorIdx - 1
	}
	return info, nil
}

func (s *TapeStore) Reset(sessionID string, archive bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.filePath(sessionID)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "tape already empty", nil
		}
		return "", err
	}

	if archive {
		archiveDir := filepath.Join(s.root, "archive")
		if err := os.MkdirAll(archiveDir, 0o755); err != nil {
			return "", err
		}
		archived := filepath.Join(
			archiveDir,
			fmt.Sprintf("%s-%s.jsonl", filepath.Base(strings.TrimSuffix(path, ".jsonl")), time.Now().UTC().Format("20060102T150405Z")),
		)
		if err := os.Rename(path, archived); err != nil {
			return "", err
		}
		s.tapeFileLocked(sessionID).reset()
		return "tape archived and reset", nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	s.tapeFileLocked(sessionID).reset()
	return "tape reset", nil
}

func (s *TapeStore) readAll(sessionID string) ([]TapeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	file := s.tapeFileLocked(sessionID)
	path := s.filePath(sessionID)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			file.reset()
			return nil, nil
		}
		return nil, err
	}
	if info.Size() < file.offset {
		// The tape file was truncated/replaced; cached entries are stale.
		file.reset()
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			file.reset()
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	if _, err := f.Seek(file.offset, io.SeekStart); err != nil {
		return nil, err
	}

	reader := bufio.NewReader(f)
	for {
		rawLine, readErr := reader.ReadBytes('\n')
		if len(rawLine) > 0 {
			file.offset += int64(len(rawLine))
			line := bytes.TrimSpace(rawLine)
			if len(line) > 0 {
				var rec TapeRecord
				if err := json.Unmarshal(line, &rec); err == nil {
					file.records = append(file.records, rec)
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}

	out := make([]TapeRecord, len(file.records))
	copy(out, file.records)
	return out, nil
}

func (s *TapeStore) tapeFileLocked(sessionID string) *tapeFile {
	if f, ok := s.files[sessionID]; ok {
		return f
	}
	f := &tapeFile{}
	s.files[sessionID] = f
	return f
}

func (s *TapeStore) filePath(sessionID string) string {
	sum := sha1.Sum([]byte(sessionID)) // #nosec G401
	name := fmt.Sprintf("%s.jsonl", hex.EncodeToString(sum[:]))
	return filepath.Join(s.root, name)
}

func encodeMessageParts(parts []blades.Part) []map[string]any {
	if len(parts) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch v := part.(type) {
		case blades.TextPart:
			out = append(out, map[string]any{
				"type": "text",
				"text": v.Text,
			})
		case blades.ToolPart:
			out = append(out, map[string]any{
				"type":     "tool",
				"id":       v.ID,
				"name":     v.Name,
				"request":  v.Request,
				"response": v.Response,
			})
		}
	}
	return out
}

func extractToolParts(parts []blades.Part) []blades.ToolPart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]blades.ToolPart, 0, len(parts))
	for _, part := range parts {
		if toolPart, ok := part.(blades.ToolPart); ok {
			out = append(out, toolPart)
		}
	}
	return out
}

func encodeToolCalls(parts []blades.ToolPart) []map[string]any {
	if len(parts) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		out = append(out, map[string]any{
			"id":        part.ID,
			"name":      part.Name,
			"arguments": part.Request,
		})
	}
	return out
}

func encodeToolResults(parts []blades.ToolPart) []any {
	if len(parts) == 0 {
		return nil
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		out = append(out, part.Response)
	}
	return out
}

func decodeMessagePayload(payload map[string]any) *blades.Message {
	if payload == nil {
		return nil
	}
	msg := &blades.Message{
		ID:           strings.TrimSpace(stringFromAny(payload["message_id"])),
		InvocationID: strings.TrimSpace(stringFromAny(payload["invocation_id"])),
		Role:         blades.Role(strings.TrimSpace(stringFromAny(payload["role"]))),
		Author:       strings.TrimSpace(stringFromAny(payload["author"])),
		Status:       blades.Status(strings.TrimSpace(stringFromAny(payload["status"]))),
		FinishReason: strings.TrimSpace(stringFromAny(payload["finish_reason"])),
	}
	if msg.ID == "" {
		msg.ID = blades.NewMessageID()
	}
	parts := decodeMessageParts(payload["parts"])
	if len(parts) == 0 {
		if text := strings.TrimSpace(stringFromAny(payload["text"])); text != "" {
			parts = append(parts, blades.TextPart{Text: text})
		}
	}
	msg.Parts = parts
	if msg.Role == "" {
		msg.Role = blades.RoleAssistant
	}
	return msg
}

func decodeMessageParts(value any) []blades.Part {
	rawList, ok := value.([]any)
	if !ok || len(rawList) == 0 {
		return nil
	}
	parts := make([]blades.Part, 0, len(rawList))
	for _, item := range rawList {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch strings.TrimSpace(stringFromAny(obj["type"])) {
		case "text":
			parts = append(parts, blades.TextPart{
				Text: stringFromAny(obj["text"]),
			})
		case "tool":
			parts = append(parts, blades.ToolPart{
				ID:       stringFromAny(obj["id"]),
				Name:     stringFromAny(obj["name"]),
				Request:  stringFromAny(obj["request"]),
				Response: stringFromAny(obj["response"]),
			})
		}
	}
	return parts
}

func selectTapeMessages(records []TapeRecord) []*blades.Message {
	if len(records) == 0 {
		return nil
	}
	out := make([]*blades.Message, 0, len(records))
	var pendingCalls []blades.ToolPart
	for _, rec := range records {
		switch rec.Kind {
		case "message":
			msg := decodeMessagePayload(rec.Payload)
			if msg != nil {
				out = append(out, msg)
			}
		case "tool_call":
			pendingCalls = decodeToolCalls(rec.Payload["calls"])
		case "tool_result":
			msg := buildToolMessage(rec.Payload, pendingCalls, rec.Payload["results"])
			if msg != nil {
				out = append(out, msg)
			}
			pendingCalls = nil
		}
	}
	return out
}

func selectRecordsAfterLastAnchor(records []TapeRecord) ([]TapeRecord, error) {
	if len(records) == 0 {
		return nil, errTapeAnchorNotFound
	}
	lastAnchorIdx := -1
	for i := len(records) - 1; i >= 0; i-- {
		if isAnchorRecord(records[i]) {
			lastAnchorIdx = i
			break
		}
	}
	if lastAnchorIdx < 0 {
		return nil, errTapeAnchorNotFound
	}
	if lastAnchorIdx+1 >= len(records) {
		return nil, nil
	}
	out := make([]TapeRecord, len(records[lastAnchorIdx+1:]))
	copy(out, records[lastAnchorIdx+1:])
	return out, nil
}

func isAnchorRecord(rec TapeRecord) bool {
	return rec.Kind == "anchor" || rec.Kind == "handoff"
}

func decodeToolCalls(value any) []blades.ToolPart {
	items := anySlice(value)
	if len(items) == 0 {
		return nil
	}
	out := make([]blades.ToolPart, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, blades.ToolPart{
			ID:      stringFromAny(obj["id"]),
			Name:    stringFromAny(obj["name"]),
			Request: stringFromAny(obj["arguments"]),
		})
	}
	return out
}

func buildToolMessage(payload map[string]any, pending []blades.ToolPart, resultValue any) *blades.Message {
	results := anySlice(resultValue)
	total := len(pending)
	if len(results) > total {
		total = len(results)
	}
	if total == 0 {
		return nil
	}
	parts := make([]blades.Part, 0, total)
	for i := 0; i < total; i++ {
		part := blades.ToolPart{}
		if i < len(pending) {
			part.ID = pending[i].ID
			part.Name = pending[i].Name
			part.Request = pending[i].Request
		}
		if i < len(results) {
			part.Response = renderToolResult(results[i])
		}
		if part.ID == "" && part.Name == "" && part.Request == "" && part.Response == "" {
			continue
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil
	}
	msg := &blades.Message{
		ID:           strings.TrimSpace(stringFromAny(payload["message_id"])),
		InvocationID: strings.TrimSpace(stringFromAny(payload["invocation_id"])),
		Role:         blades.RoleTool,
		Author:       strings.TrimSpace(stringFromAny(payload["author"])),
		Status:       blades.Status(strings.TrimSpace(stringFromAny(payload["status"]))),
		FinishReason: strings.TrimSpace(stringFromAny(payload["finish_reason"])),
		Parts:        parts,
	}
	if msg.ID == "" {
		msg.ID = blades.NewMessageID()
	}
	if msg.Status == "" {
		msg.Status = blades.StatusCompleted
	}
	return msg
}

func anySlice(value any) []any {
	if value == nil {
		return nil
	}
	if out, ok := value.([]any); ok {
		return out
	}
	return nil
}

func renderToolResult(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return stringFromAny(v)
		}
		return string(b)
	}
}

func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case fmt.Stringer:
		return x.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}
