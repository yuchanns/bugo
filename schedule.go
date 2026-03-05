package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
)

type ScheduledJob struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	ChatID    int64     `json:"chat_id"`
	Cron      string    `json:"cron"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

type ScheduleStore struct {
	scheduler gocron.Scheduler
	onFire    func(sessionID string, chatID int64, message string)

	mu      sync.Mutex
	jobs    map[string]ScheduledJob
	jobUUID map[string]uuid.UUID
}

func NewScheduleStore(onFire func(sessionID string, chatID int64, message string)) (*ScheduleStore, error) {
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}
	scheduler.Start()

	return &ScheduleStore{
		scheduler: scheduler,
		onFire:    onFire,
		jobs:      map[string]ScheduledJob{},
		jobUUID:   map[string]uuid.UUID{},
	}, nil
}

func (s *ScheduleStore) Close() {
	if s == nil || s.scheduler == nil {
		return
	}
	_ = s.scheduler.Shutdown()
}

func (s *ScheduleStore) Add(sessionID string, chatID int64, spec, message, customID string) (ScheduledJob, error) {
	spec = strings.TrimSpace(spec)
	message = strings.TrimSpace(message)
	customID = strings.TrimSpace(customID)
	if spec == "" {
		return ScheduledJob{}, fmt.Errorf("cron is required")
	}
	if message == "" {
		return ScheduledJob{}, fmt.Errorf("message is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	jobID := customID
	if jobID == "" {
		jobID = "job-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	}
	if _, exists := s.jobs[jobID]; exists {
		return ScheduledJob{}, fmt.Errorf("job already exists: %s", jobID)
	}

	job := ScheduledJob{
		ID:        jobID,
		SessionID: sessionID,
		ChatID:    chatID,
		Cron:      spec,
		Message:   message,
		CreatedAt: time.Now().UTC(),
	}

	created, err := s.scheduler.NewJob(
		gocron.CronJob(spec, false),
		gocron.NewTask(func() {
			if s.onFire != nil {
				s.onFire(job.SessionID, job.ChatID, job.Message)
			}
		}),
		gocron.WithName(jobID),
	)
	if err != nil {
		return ScheduledJob{}, err
	}

	s.jobs[jobID] = job
	s.jobUUID[jobID] = created.ID()
	return job, nil
}

func (s *ScheduleStore) Remove(sessionID, jobID string) (bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return false, fmt.Errorf("job_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[jobID]
	if !ok {
		return false, nil
	}
	if job.SessionID != sessionID {
		return false, fmt.Errorf("job does not belong to current session")
	}

	if id, exists := s.jobUUID[jobID]; exists {
		if err := s.scheduler.RemoveJob(id); err != nil && !errors.Is(err, gocron.ErrJobNotFound) {
			return false, err
		}
	}
	delete(s.jobUUID, jobID)
	delete(s.jobs, jobID)
	return true, nil
}

func (s *ScheduleStore) List(sessionID string) []ScheduledJob {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]ScheduledJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		if job.SessionID != sessionID {
			continue
		}
		out = append(out, job)
	}
	return out
}
