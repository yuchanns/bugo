package main

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/go-kratos/blades"
)

type TapeSession struct {
	id      string
	state   blades.State
	history []*blades.Message
	tapes   *TapeStore
	mu      sync.RWMutex
}

func NewTapeSession(id string, tapes *TapeStore) *TapeSession {
	return &TapeSession{
		id:    id,
		state: blades.State{},
		tapes: tapes,
	}
}

func (s *TapeSession) ID() string {
	return s.id
}

func (s *TapeSession) State() blades.State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return blades.State(maps.Clone(s.state))
}

func (s *TapeSession) SetState(key string, value any) {
	s.mu.Lock()
	s.state[key] = value
	s.mu.Unlock()
	_ = s.tapes.Append(s.id, "state.set", map[string]any{
		"key":   key,
		"value": value,
	})
}

func (s *TapeSession) History() []*blades.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.history)
}

func (s *TapeSession) Append(ctx context.Context, message *blades.Message) error {
	if message == nil {
		return nil
	}
	s.mu.Lock()
	s.history = append(s.history, cloneMessage(message))
	s.mu.Unlock()

	return s.tapes.Append(s.id, "message", map[string]any{
		"time":          time.Now().UTC().Format(time.RFC3339Nano),
		"message_id":    message.ID,
		"role":          string(message.Role),
		"author":        message.Author,
		"status":        string(message.Status),
		"finish_reason": message.FinishReason,
		"text":          message.Text(),
	})
}

func (s *TapeSession) Reset() {
	s.mu.Lock()
	s.state = blades.State{}
	s.history = nil
	s.mu.Unlock()
}

func cloneMessage(in *blades.Message) *blades.Message {
	if in == nil {
		return nil
	}
	out := in.Clone()
	out.Parts = slices.Clone(in.Parts)
	out.Actions = maps.Clone(in.Actions)
	out.Metadata = maps.Clone(in.Metadata)
	return out
}

type SessionStore struct {
	tapes *TapeStore
	mu    sync.Mutex
	sets  map[string]*TapeSession
}

func NewSessionStore(tapes *TapeStore) *SessionStore {
	return &SessionStore{
		tapes: tapes,
		sets:  map[string]*TapeSession{},
	}
}

func (s *SessionStore) Get(sessionID string) *TapeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.sets[sessionID]; ok {
		return existing
	}
	created := NewTapeSession(sessionID, s.tapes)
	_ = s.tapes.Append(sessionID, "session.start", map[string]any{
		"session_id": sessionID,
		"started_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
	s.sets[sessionID] = created
	return created
}

type sessionInbox struct {
	session     *TapeSession
	chatID      int64
	lastMention time.Time
	pending     []string
	timer       *time.Timer
	running     bool
	mu          sync.Mutex
}

type inboxHub struct {
	sessions *SessionStore
	mu       sync.Mutex
	items    map[string]*sessionInbox
}

func newInboxHub(sessions *SessionStore) *inboxHub {
	return &inboxHub{
		sessions: sessions,
		items:    map[string]*sessionInbox{},
	}
}

func (h *inboxHub) Get(sessionID string, chatID int64) *sessionInbox {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.items[sessionID]; ok {
		existing.chatID = chatID
		return existing
	}

	created := &sessionInbox{
		session: h.sessions.Get(sessionID),
		chatID:  chatID,
	}
	h.items[sessionID] = created
	return created
}

func (h *inboxHub) resetSession(sessionID string) bool {
	h.mu.Lock()
	inbox, ok := h.items[sessionID]
	h.mu.Unlock()
	if !ok || inbox == nil {
		return false
	}
	return inbox.resetRuntime()
}

func (inbox *sessionInbox) resetRuntime() bool {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()

	if inbox.timer != nil {
		inbox.timer.Stop()
		inbox.timer = nil
	}
	inbox.pending = nil
	inbox.lastMention = time.Time{}
	return false
}
