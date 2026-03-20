package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	path      string

	mu      sync.Mutex
	jobs    map[string]ScheduledJob
	jobUUID map[string]uuid.UUID
}

func NewScheduleStore(path string, onFire func(sessionID string, chatID int64, message string)) (*ScheduleStore, error) {
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return nil, err
	}
	store := &ScheduleStore{
		scheduler: scheduler,
		onFire:    onFire,
		path:      path,
		jobs:      map[string]ScheduledJob{},
		jobUUID:   map[string]uuid.UUID{},
	}
	if err := store.load(); err != nil {
		_ = scheduler.Shutdown()
		return nil, err
	}
	scheduler.Start()
	return store, nil
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

	created, err := s.registerJob(job)
	if err != nil {
		return ScheduledJob{}, err
	}

	s.jobs[jobID] = job
	s.jobUUID[jobID] = created.ID()
	if err := s.saveLocked(); err != nil {
		if removeErr := s.scheduler.RemoveJob(created.ID()); removeErr != nil && !errors.Is(removeErr, gocron.ErrJobNotFound) {
			return ScheduledJob{}, errors.Join(err, removeErr)
		}
		delete(s.jobs, jobID)
		delete(s.jobUUID, jobID)
		return ScheduledJob{}, err
	}
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
	if err := s.saveLocked(); err != nil {
		return false, err
	}
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

func (s *ScheduleStore) load() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var jobs []ScheduledJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return fmt.Errorf("load schedules: %w", err)
	}
	for _, job := range jobs {
		job = normalizeScheduledJob(job)
		if job.ID == "" {
			return fmt.Errorf("load schedules: job_id is required")
		}
		created, err := s.registerJob(job)
		if err != nil {
			return fmt.Errorf("load schedules %s: %w", job.ID, err)
		}
		s.jobs[job.ID] = job
		s.jobUUID[job.ID] = created.ID()
	}
	return nil
}

func (s *ScheduleStore) registerJob(job ScheduledJob) (gocron.Job, error) {
	return s.scheduler.NewJob(
		gocron.CronJob(job.Cron, false),
		gocron.NewTask(func() {
			if s.onFire != nil {
				s.onFire(job.SessionID, job.ChatID, job.Message)
			}
		}),
		gocron.WithName(job.ID),
	)
}

func (s *ScheduleStore) saveLocked() error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	jobs := make([]ScheduledJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, normalizeScheduledJob(job))
	}
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].ID < jobs[j].ID
	})
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := s.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}

func normalizeScheduledJob(job ScheduledJob) ScheduledJob {
	job.ID = strings.TrimSpace(job.ID)
	job.SessionID = strings.TrimSpace(job.SessionID)
	job.Cron = strings.TrimSpace(job.Cron)
	job.Message = strings.TrimSpace(job.Message)
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	} else {
		job.CreatedAt = job.CreatedAt.UTC()
	}
	return job
}
