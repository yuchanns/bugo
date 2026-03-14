package main

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/go-kratos/blades"
	log "github.com/yuchanns/bugo/internal/logging"
)

type TapeSession struct {
	id    string
	state blades.State
	tapes *TapeStore
	mu    sync.RWMutex
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
	history, err := s.tapes.HistoryMessages(s.id)
	if err != nil {
		log.Error().
			Str("session_id", s.id).
			Err(err).
			Msg("session.history.load.failed")
		return nil
	}
	return history
}

func (s *TapeSession) Append(ctx context.Context, message *blades.Message) error {
	_ = ctx
	return s.tapes.AppendMessage(s.id, message)
}

func (s *TapeSession) Reset() {
	s.mu.Lock()
	s.state = blades.State{}
	s.mu.Unlock()
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
	_ = s.tapes.EnsureBootstrapAnchor(sessionID)
	s.sets[sessionID] = created
	return created
}

type sessionInbox struct {
	session        *TapeSession
	chatID         int64
	lastMention    time.Time
	pending        []string
	interrupts     []string
	segmentVersion int
	timer          *time.Timer
	running        bool
	mu             sync.Mutex
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

func (h *inboxHub) Find(sessionID string) *sessionInbox {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.items[sessionID]
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
	inbox.interrupts = nil
	inbox.segmentVersion = 0
	inbox.lastMention = time.Time{}
	return false
}

func (inbox *sessionInbox) enqueueInterrupt(prompt string) {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	inbox.interrupts = append(inbox.interrupts, prompt)
}

func (inbox *sessionInbox) consumeInterrupts() ([]string, int) {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if len(inbox.interrupts) == 0 {
		return nil, inbox.segmentVersion
	}
	out := append([]string(nil), inbox.interrupts...)
	inbox.interrupts = nil
	inbox.segmentVersion++
	return out, inbox.segmentVersion
}

func (inbox *sessionInbox) segment() int {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	return inbox.segmentVersion
}

func (inbox *sessionInbox) drainInterruptsToPending() int {
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if len(inbox.interrupts) == 0 {
		return 0
	}
	inbox.pending = append(inbox.pending, inbox.interrupts...)
	drained := len(inbox.interrupts)
	inbox.interrupts = nil
	return drained
}
