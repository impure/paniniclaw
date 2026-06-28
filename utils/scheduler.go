package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/robfig/cron/v3"
)

type Job struct {
	Model    string `json:"model,omitempty" toml:"model,omitempty"`
	Schedule string `json:"schedule" toml:"schedule"`
	Name     string `json:"name" toml:"name"`
	Task     string `json:"task" toml:"task"`
	Notify   bool   `json:"notify,omitempty" toml:"notify,omitempty"`
}

type LLMClient interface {
	Chat(ctx context.Context, task string, model string, onMessage func(string)) (string, error)
	ChatJSON(ctx context.Context, systemPrompt string) (string, error)
}

type MessageSender func(chatId, text string)

type Scheduler struct {
	dir         string
	client      LLMClient
	send        MessageSender
	chatId      string
	lastRuns    map[string]time.Time
	mu          sync.Mutex
	currentTask string // name of the currently active task, empty if none
	taskMu      sync.RWMutex
}

func NewScheduler(dir string, client LLMClient, send MessageSender, chatId string) *Scheduler {
	return &Scheduler{
		dir:      dir,
		client:   client,
		send:     send,
		chatId:   chatId,
		lastRuns: make(map[string]time.Time),
	}
}

func (s *Scheduler) Start() {
	os.MkdirAll(s.dir, 0755)
	log.Printf("[scheduler] Watching directory: %s", s.dir)
	go s.loop()
}

func (s *Scheduler) loop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	s.checkAndRun()

	for range ticker.C {
		s.checkAndRun()
	}
}

func (s *Scheduler) GetCurrentTask() string {
	s.taskMu.RLock()
	defer s.taskMu.RUnlock()
	return s.currentTask
}

func (s *Scheduler) EndTask() {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	if s.currentTask != "" {
		log.Printf("[scheduler] Ending task %q", s.currentTask)
		s.currentTask = ""
	}
}

func (s *Scheduler) setTask(name string) {
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	s.currentTask = name
}

// isTaskFile checks if a filename is a supported task file (.json or .toml)
func isTaskFile(name string) bool {
	return strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".toml")
}

// trimTaskExt removes the .json or .toml extension from a filename
func trimTaskExt(name string) string {
	return strings.TrimSuffix(strings.TrimSuffix(name, ".json"), ".toml")
}

// ListTaskNames returns the names (without extension) of all available tasks.
func (s *Scheduler) ListTaskNames() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && isTaskFile(entry.Name()) {
			names = append(names, trimTaskExt(entry.Name()))
		}
	}
	return names, nil
}

// RunTaskByName loads a task by name (without extension) and runs it immediately.
// Returns an error if the task is not found or if another task is currently running.
func (s *Scheduler) RunTaskByName(name string) error {
	if s.GetCurrentTask() != "" {
		return fmt.Errorf("task %q is already running, wait for it to finish or use /end_task", s.GetCurrentTask())
	}

	// Try .toml first, then .json
	job, err := loadTask(s.dir, name)
	if err != nil {
		return fmt.Errorf("task %q not found: %v", name, err)
	}

	displayName := name
	if job.Name != "" {
		displayName = job.Name
	}
	s.setTask(displayName)

	go s.runTask(displayName, job.Task, job.Model, job.Notify)
	return nil
}

func (s *Scheduler) runTask(name, prompt, model string, notify bool) {
	defer s.EndTask()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log.Printf("[scheduler] Sending LLM request for task %q", name)

	msgCount := 0
	result, err := s.client.Chat(ctx, prompt, model, func(msg string) {
		if notify {
			msgCount++
			var full string
			if msgCount == 1 {
				full = fmt.Sprintf("📋 Task %q:\n%s", name, msg)
			} else {
				full = fmt.Sprintf("📋 Task %q (continued):\n%s", name, msg)
			}
			if s.send != nil {
				s.send(s.chatId, full)
			}
		}
	})
	if err != nil {
		// Check if this is an explicit task failure (via end_task with reason)
		errStr := err.Error()
		if strings.HasPrefix(errStr, "task failed: ") {
			reason := strings.TrimPrefix(errStr, "task failed: ")
			debrief := fmt.Sprintf("❌ Task %q failed: %s", name, reason)
			log.Printf("[scheduler] %s", debrief)
			if s.send != nil {
				s.send(s.chatId, debrief)
			}
		} else {
			debrief := fmt.Sprintf("⚠️ Task %q error: %v", name, err)
			log.Printf("[scheduler] %s", debrief)
			if s.send != nil {
				s.send(s.chatId, debrief)
			}
		}
	} else {
		// Summarize into a brief debrief
		debrief := fmt.Sprintf("✅ Task %q completed.", name)
		// Truncate result for debrief if needed
		if len(result) > 500 {
			debrief = fmt.Sprintf("✅ Task %q completed.\n\n%s...", name, result[:500])
		} else if result != "" {
			debrief = fmt.Sprintf("✅ Task %q completed.\n\n%s", name, result)
		}
		log.Printf("[scheduler] %s", debrief)
		if s.send != nil {
			s.send(s.chatId, debrief)
		}
	}
}

func (s *Scheduler) checkAndRun() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}

	now := time.Now()

	for _, entry := range entries {
		if entry.IsDir() || !isTaskFile(entry.Name()) {
			continue
		}

		path := filepath.Join(s.dir, entry.Name())
		job, err := loadJob(path)
		if err != nil {
			log.Printf("[scheduler] %s: %v", entry.Name(), err)
			continue
		}

		// Use robfig/cron to check if the schedule matches the current time
		sched, err := cron.ParseStandard(job.Schedule)
		if err != nil {
			log.Printf("[scheduler] %s: invalid schedule %q: %v", entry.Name(), job.Schedule, err)
			continue
		}

		// A schedule matches if the next time it fires is within this minute
		next := sched.Next(now.Truncate(time.Minute).Add(-time.Minute))
		if next.After(now) || next.Before(now.Truncate(time.Minute)) {
			continue
		}

		jobName := trimTaskExt(entry.Name())

		s.mu.Lock()
		lastRun, exists := s.lastRuns[jobName]
		s.mu.Unlock()

		if exists && now.Sub(lastRun) < time.Minute {
			continue
		}

		log.Printf("[scheduler] Running task %q (%s)", jobName, job.Schedule)

		if job.Task != "" && s.client != nil {
			if s.GetCurrentTask() != "" {
				log.Printf("[scheduler] Task %q already active, skipping job %q", s.GetCurrentTask(), jobName)
				continue
			}

			displayName := jobName
			if job.Name != "" {
				displayName = job.Name
			}
			s.setTask(displayName)

			go s.runTask(displayName, job.Task, job.Model, job.Notify)
		}

		s.mu.Lock()
		s.lastRuns[jobName] = now
		s.mu.Unlock()
	}
}

// loadTask tries to load a task by name, checking .toml first then .json
func loadTask(dir, name string) (Job, error) {
	// Try .toml first
	tomlPath := filepath.Join(dir, name+".toml")
	if _, err := os.Stat(tomlPath); err == nil {
		return loadJob(tomlPath)
	}

	// Fall back to .json
	jsonPath := filepath.Join(dir, name+".json")
	return loadJob(jsonPath)
}

func loadJob(path string) (Job, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Job{}, err
	}

	var job Job

	if strings.HasSuffix(path, ".toml") {
		if err := toml.Unmarshal(data, &job); err != nil {
			return Job{}, fmt.Errorf("invalid TOML: %w", err)
		}
	} else {
		if err := json.Unmarshal(data, &job); err != nil {
			return Job{}, fmt.Errorf("invalid JSON: %w", err)
		}
	}

	if job.Schedule == "" {
		return Job{}, fmt.Errorf("missing 'schedule' key")
	}

	return job, nil
}
