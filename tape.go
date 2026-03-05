package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	root string
	mu   sync.Mutex
}

func NewTapeStore(root string) (*TapeStore, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &TapeStore{root: root}, nil
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
		if rec.Kind == "anchor" || rec.Kind == "handoff" {
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
		if rec.Kind != "anchor" && rec.Kind != "handoff" {
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
		return "tape archived and reset", nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return "tape reset", nil
}

func (s *TapeStore) readAll(sessionID string) ([]TapeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.filePath(sessionID)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	out := make([]TapeRecord, 0, 128)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec TapeRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *TapeStore) filePath(sessionID string) string {
	sum := sha1.Sum([]byte(sessionID)) // #nosec G401
	name := fmt.Sprintf("%s.jsonl", hex.EncodeToString(sum[:]))
	return filepath.Join(s.root, name)
}
