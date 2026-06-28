package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type MemoryManager struct {
	db        *Database
	users     *UserStore
	llm       LLMClient
	memoryDir string
}

func NewMemoryManager(db *Database, users *UserStore, llm LLMClient) *MemoryManager {
	dir := "memory"
	os.MkdirAll(dir, 0755)
	return &MemoryManager{
		db:        db,
		users:     users,
		llm:       llm,
		memoryDir: dir,
	}
}

// Run performs one cycle of memory summarization.
// It iterates over each (user, connection) pair, skips any that have had
// messages in the last 4 hours, and summarizes the earliest unsummarized
// conversation batch for the next candidate.
func (m *MemoryManager) Run() {
	log.Println("[memory] Running memory cycle")

	now := time.Now()
	fourHoursAgo := now.Add(-4 * time.Hour)

	for _, user := range m.users.data.Users {
		for _, conn := range user.Connections {
			provider := conn.Provider
			chatID, ok := conn.Data["chat_id"]
			if !ok {
				continue
			}
			conversationID := fmt.Sprintf("%d", ToInt64(chatID))

			// Skip if there are messages in the last 4 hours (actively chatting)
			recentCount, err := m.db.CountMessagesSince(provider, conversationID, fourHoursAgo)
			if err != nil {
				log.Printf("[memory] Error checking recent messages for %s/%s: %v", provider, conversationID, err)
				continue
			}
			if recentCount > 0 {
				log.Printf("[memory] Skipping %s/%s — active in last 4 hours (%d messages)", provider, conversationID, recentCount)
				continue
			}

			// Get the last summary time for this conversation
			lastSummaryTime := m.getLastSummaryTime(provider, conversationID)

			// Find the earliest messages after the last summary
			messages, err := m.db.GetMessagesSince(provider, conversationID, lastSummaryTime)
			if err != nil {
				log.Printf("[memory] Error fetching messages for %s/%s: %v", provider, conversationID, err)
				continue
			}

			// Find the first session boundary in these messages
			sessionMsgs := m.findFirstSession(messages)
			if len(sessionMsgs) == 0 {
				log.Printf("[memory] No messages to summarize for %s/%s", provider, conversationID)
				continue
			}

			// Summarize
			log.Printf("[memory] Summarizing %d messages for %s/%s", len(sessionMsgs), provider, conversationID)
			summary, err := m.summarizeConversation(sessionMsgs)
			if err != nil {
				log.Printf("[memory] Failed to summarize %s/%s: %v", provider, conversationID, err)
				continue
			}

			// Store marker even if nothing important to avoid reprocessing
			if summary == "" {
				log.Printf("[memory] Nothing important in session for %s/%s, marking as processed", provider, conversationID)
				sessionEnd := sessionMsgs[len(sessionMsgs)-1].CreatedAt
				if err := m.storeSummary(provider, conversationID, sessionEnd, ""); err != nil {
					log.Printf("[memory] Failed to store empty summary for %s/%s: %v", provider, conversationID, err)
				}
			} else {
				sessionEnd := sessionMsgs[len(sessionMsgs)-1].CreatedAt
				if err := m.storeSummary(provider, conversationID, sessionEnd, summary); err != nil {
					log.Printf("[memory] Failed to store summary for %s/%s: %v", provider, conversationID, err)
					continue
				}
				log.Printf("[memory] Stored summary for %s/%s (%d messages)", provider, conversationID, len(sessionMsgs))
			}

			// Only do one conversation per cycle
			return
		}
	}

	log.Println("[memory] No conversations to summarize")
}

// getLastSummaryTime returns the time of the most recent summary for a conversation.
// Returns a zero time if none exists.
func (m *MemoryManager) getLastSummaryTime(provider string, conversationID string) time.Time {
	path := filepath.Join(m.memoryDir, m.filename(provider, conversationID))
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}

	lines := strings.Split(string(data), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "## Summary — ") {
			// Parse date: "## Summary — 2026-07-20 14:30:00"
			dateStr := strings.TrimPrefix(line, "## Summary — ")
			t, err := time.Parse("2006-01-02 15:04:05", dateStr)
			if err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// filename returns the memory file name for a provider+conversation pair.
func (m *MemoryManager) filename(provider, conversationID string) string {
	return fmt.Sprintf("%s_%s.md", provider, conversationID)
}

// findFirstSession finds the earliest complete session (bounded by a 4h gap) in messages.
// Returns at least 2 messages if available (or all if fewer). Falls back to all messages
// if no gap is found so we don't get stuck forever.
func (m *MemoryManager) findFirstSession(messages []Message) []Message {
	if len(messages) < 2 {
		return messages
	}

	end := len(messages)
	for i := 1; i < len(messages); i++ {
		if messages[i].Data.Role == "user" {
			gap := messages[i].CreatedAt.Sub(messages[i-1].CreatedAt)
			if gap > 4*time.Hour {
				end = i
				break
			}
		}
	}

	// If no gap found, summarize everything we have
	return messages[:end]
}

// summarizeConversation sends the messages to the LLM for summarization.
// Returns bullet points of key facts. Returns empty string if nothing important was discussed.
func (m *MemoryManager) summarizeConversation(messages []Message) (string, error) {
	// Build a compact transcript
	var transcript strings.Builder
	for _, msg := range messages {
		role := msg.Data.Role
		var content string
		switch v := msg.Data.Content.(type) {
		case string:
			content = v
		default:
			content = fmt.Sprintf("%v", v)
		}
		if content == "" {
			continue
		}
		transcript.WriteString(fmt.Sprintf("[%s]: %s\n", role, content))
	}

	prompt := fmt.Sprintf(`Review the following conversation and extract any important information the user shared. This could include: preferences, plans, technical details, decisions, personal information, questions they want answered later, or anything worth remembering.

If nothing important was discussed (just casual chat, greetings, etc.), respond with exactly: {"points": []}

Otherwise, respond with a JSON object containing a single key "points" with an array of strings, one per key point. Write in third person about "the user". Keep each point brief. Do not include any introductory text, explanations, or commentary — just the JSON.

Conversation:
%s

JSON:`, transcript.String())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	result, err := m.llm.ChatJSON(ctx, prompt)
	if err != nil {
		return "", fmt.Errorf("summarization failed: %w", err)
	}

	result = strings.TrimSpace(result)

	// Log raw result for debugging
	log.Printf("[memory] Raw LLM result (first 200 chars): %s", result[:min(len(result), 200)])

	// Try to parse as JSON
	var parsed struct {
		Points []string `json:"points"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err == nil {
		if len(parsed.Points) == 0 {
			return "", nil
		}
		var sb strings.Builder
		for _, p := range parsed.Points {
			sb.WriteString("- " + p + "\n")
		}
		return strings.TrimSuffix(sb.String(), "\n"), nil
	}

	log.Printf("[memory] Failed to parse JSON from LLM response: %v", err)
	return "", nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// storeSummary appends a summary entry to the memory markdown file.
func (m *MemoryManager) storeSummary(provider, conversationID string, sessionEnd time.Time, summary string) error {
	path := filepath.Join(m.memoryDir, m.filename(provider, conversationID))

	log.Printf("[memory] storeSummary: opening %s for append (summary len=%d)", path, len(summary))

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[memory] storeSummary: failed to open: %v", err)
		return err
	}
	defer f.Close()

	timestamp := sessionEnd.Format("2006-01-02 15:04:05")
	if summary == "" {
		entry := fmt.Sprintf("\n## Summary — %s\n*Nothing important*\n", timestamp)
		n, err := f.WriteString(entry)
		log.Printf("[memory] storeSummary: wrote empty summary (%d bytes): %v", n, err)
		return err
	}

	entry := fmt.Sprintf("\n## Summary — %s\n%s\n", timestamp, summary)
	n, err := f.WriteString(entry)
	log.Printf("[memory] storeSummary: wrote %d bytes, err=%v", n, err)

	// Also check file size after write
	if err == nil {
		info, _ := os.Stat(path)
		if info != nil {
			log.Printf("[memory] storeSummary: file size now %d bytes", info.Size())
		}
	}

	return err
}
