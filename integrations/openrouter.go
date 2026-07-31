package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"paniniclaw/utils"
	"path/filepath"
	"strconv"
)

type OpenRouter struct {
	apiKey string
	model  string
}

func NewOpenRouter(apiKey string) *OpenRouter {
	return &OpenRouter{
		apiKey: apiKey,
		model:  "deepseek/deepseek-v4-flash",
	}
}

type reasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}

// providerConfig allows specifying routing preferences to OpenRouter
type providerConfig struct {
	Order          []string `json:"order,omitempty"`
	AllowFallbacks *bool    `json:"allow_fallbacks,omitempty"`
}

type responseFormatConfig struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string               `json:"model"`
	Messages       []utils.ChatMessage  `json:"messages"`
	Reasoning      *reasoningConfig     `json:"reasoning,omitempty"`
	ResponseFormat *responseFormatConfig `json:"response_format,omitempty"`
	MaxTokens      int                  `json:"max_tokens,omitempty"`
	Tools          []Tool               `json:"tools,omitempty"`
	Provider       *providerConfig      `json:"provider,omitempty"` // Added for provider configuration
}

type chatResponse struct {
	Choices []struct {
		Message utils.ChatMessage `json:"message"`
	} `json:"choices"`
}

func makeSystemMessage(user utils.User, chatId string) (utils.ChatMessage, error) {
	soulBytes, err := ensureFileExists("directives/core.md", `You are PaniniClaw, a helpful AI assistant that also makes paninis.
- You are running on the paniniclaw service
- When responding to user requests, briefly explain what command/action you're about to perform. For example: "I'll search the web for X", "I'll check git status", etc. This helps avoid confusion about what's happening
- If you're unsure about something ask for clarification instead of guessing
- You may edit this file with user permission
- Always ask before killing/restarting processes or services or using force commands
- You do not need permission to edit files in the notes directory and should edit them with any information that might be useful later
- The user can make mistakes, especially about programming. You should double check the user doesn't accidentally break something.`)
	if err != nil {
		return utils.ChatMessage{}, err
	}

	telegramBytes, err := ensureFileExists("directives/telegram.md", "You are operating in a telegram chat. Do not use markdown as it is not supported.")
	if err != nil {
		return utils.ChatMessage{}, err
	}

	generalBytes, err := ensureFileExists("notes/general.md", `- When executing terminal tasks you are an unprivileged user.
- If you are unable to do something do not go around in circles trying complicated workarounds. Instead you should ask the user for help.
- Always be extremely careful with commands that can delete data. This includes commands which may overwrite files.
- Do not use curl directly as it wastes tokens and takes forever, instead you can run ./clean_curl.py <url> which strips HTML tags.
- When editing Go code make sure to format your code after`)
	if err != nil {
		return utils.ChatMessage{}, err
	}

	userJson, err := user.MakeJson()
	if err != nil {
		return utils.ChatMessage{}, err
	}

	userNotesBytes, err := ensureFileExists(fmt.Sprintf("notes/user%s.md", strconv.Itoa(user.Id)), "")
	if err != nil {
		return utils.ChatMessage{}, err
	}

	// Load memory file for this conversation if it exists
	provider := "telegram"
	if len(user.Connections) > 0 {
		provider = user.Connections[0].Provider
	}
	memoryPath := fmt.Sprintf("memory/%s_%s.md", provider, chatId)
	memoryBytes, memErr := os.ReadFile(memoryPath)
	var memoryContent string
	if memErr == nil {
		memoryContent = string(memoryBytes)
	}
	
	return utils.ChatMessage{
		Role:    "system",
		Content: fmt.Sprintf("directives/core.md: %s\n\ndirectives/telegram.md: %s\n\nuser: %s\n\nnotes/general.md: %s\n\nnotes/user%s.md: %s\n\nmemory/%s_%s.md: %s\n", soulBytes, telegramBytes, userJson, generalBytes, strconv.Itoa(user.Id), userNotesBytes, provider, chatId, memoryContent),
	}, nil
}

func ensureFileExists(path string, defaultContent string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Create the parent directories if they don't exist
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, err
			}

			// Write the default content
			contentBytes := []byte(defaultContent)
			if err := os.WriteFile(path, contentBytes, 0644); err != nil {
				return nil, err
			}
			return contentBytes, nil
		}
		// Return any other error encountered (e.g., permission issues)
		return nil, err
	}
	return data, nil
}

func (o *OpenRouter) ChatFromMessages(ctx context.Context, messages []utils.Message, user utils.User, db *utils.Database, chatId string) (string, error) {
	systemMessage, err := makeSystemMessage(user, chatId)
	if err != nil {
		return "", err
	}

	chatMessages := make([]utils.ChatMessage, len(messages))
	for i, msg := range messages {
		chatMessages[i] = msg.Data
	}

	allMessages := append([]utils.ChatMessage{systemMessage}, chatMessages...)
	return o.chatWithTools(ctx, allMessages, db, chatId, user)
}

func getToolCalls(m map[string]any, key string) []utils.ToolCall {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil
	}

	// 1. If it's already the correct type, return it
	if tc, ok := raw.([]utils.ToolCall); ok {
		return tc
	}

	// 2. If it was unmarshaled as generic JSON data, convert it
	bytes, err := json.Marshal(raw)
	if err != nil {
		return nil
	}

	var tc []utils.ToolCall
	if err := json.Unmarshal(bytes, &tc); err != nil {
		return nil
	}
	return tc
}

func (o *OpenRouter) chatWithTools(ctx context.Context, messages []utils.ChatMessage, db *utils.Database, chatId string, user utils.User) (string, error) {
	const maxIterations = 100
	for i := 0; i < maxIterations; i++ {
		allowFallbacks := true

		reqBody := chatRequest{
			Model:    o.model,
			Messages: messages,
			Reasoning: &reasoningConfig{
				Effort: "none",
			},
			MaxTokens: 10_000,
			Tools: []Tool{
				TerminalTool,
				AppendNotesTool,
				{
					Type: "openrouter:web_search",
				},
			},
			Provider: &providerConfig{
				Order:          []string{"novita"}, // Prioritizes Novita
				AllowFallbacks: &allowFallbacks,    // Allows other providers if Novita is down
			},
		}

		responseMsg, err := o.rawChat(ctx, reqBody)
		if err != nil {
			return "", err
		}

		// Append the assistant response message to context
		messages = append(messages, responseMsg)

		if len(responseMsg.ToolCalls) == 0 {
			// No tools called, return the text content
			return responseMsg.Content.(string), nil
		}

		if responseMsg.Content != "" {
			if contentStr, ok := responseMsg.Content.(string); ok {
				sendMessageToPrimaryAccount(contentStr, user)
			}
		}

		msgJson, _ := json.Marshal(map[string]interface{}{
			"role":       "assistant",
			"content":    responseMsg.Content,
			"tool_calls": responseMsg.ToolCalls,
		})

		db.AddMessage(
			"telegram",
			chatId,
			string(msgJson),
		)

		// Process tool calls
		for _, toolCall := range responseMsg.ToolCalls {
			switch toolCall.Function.Name {
			case "execute_command":
				var args ExecuteCommandArgs
				if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
					return "", fmt.Errorf("failed to parse tool arguments: %v", err)
				}

				print("Executing command for tool call:", args.Command, ". id:", toolCall.ID, ".")
				output, err := ExecuteCommand(args.Command)
				if err != nil {
					output = fmt.Sprintf("Error: %v\nOutput: %s", err, output)
				}
				//println("Output:", output)

				message := utils.ChatMessage{
					Role:       "tool",
					Name:       "execute_command",
					ToolCallID: toolCall.ID,
					Content:    output,
				}
				messages = append(messages, message)
				toolJson, _ := json.Marshal(message)

				db.AddMessage(
					"telegram",
					chatId,
					string(toolJson),
				)

			case "append_notes":
				var args AppendNotesArgs
				if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
					return "", fmt.Errorf("failed to parse tool arguments: %v", err)
				}

				println("Appending to notes:", args.Content, "scope:", args.Scope)
				output, err := AppendNotes(args.Content, args.Scope, user.Id)
				if err != nil {
					output = fmt.Sprintf("Error: %v", err)
				}
				println("Output:", output)

				message := utils.ChatMessage{
					Role:       "tool",
					Name:       "append_notes",
					ToolCallID: toolCall.ID,
					Content:    output,
				}
				messages = append(messages, message)
				toolJson, _ := json.Marshal(message)

				db.AddMessage(
					"telegram",
					chatId,
					string(toolJson),
				)

			default:
				message := utils.ChatMessage{
					Role:       "tool",
					Name:       toolCall.Function.Name,
					ToolCallID: toolCall.ID,
					Content:    fmt.Sprintf("Error: Unknown tool %s", toolCall.Function.Name),
				}
				messages = append(messages, message)
				toolJson, _ := json.Marshal(message)

				db.AddMessage(
					"telegram",
					chatId,
					string(toolJson),
				)

				println("Error: Unknown tool %s", toolCall.Function.Name)
			}
		}
	}

	return "", fmt.Errorf("exceeded max tool call iterations limit (%d)", maxIterations)
}

func (o *OpenRouter) rawChat(ctx context.Context, prompt chatRequest) (utils.ChatMessage, error) {
	body, err := json.Marshal(prompt)
	if err != nil {
		return utils.ChatMessage{}, err
	}

	// The body can be very large in long chats
	/* Disable this because for tool calls the output can be very long
	if len(prompt.Messages) > 0 {
		lastMessage, err := json.MarshalIndent(prompt.Messages[len(prompt.Messages)-1], "", "\t")
		if err != nil {
			println("Error marshaling last message:", err.Error())
		}
		println("Making request with last message:\n", string(lastMessage))
	}
	*/

	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		"https://openrouter.ai/api/v1/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return utils.ChatMessage{}, err
	}

	req.Header.Set("Authorization", "Bearer "+o.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OpenRouter-Title", "PaniniClaw")
	req.Header.Set("HTTP-Referer", "https://github.com/impure/paniniclaw")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return utils.ChatMessage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody bytes.Buffer
		_, _ = errBody.ReadFrom(resp.Body)
		return utils.ChatMessage{}, fmt.Errorf("openrouter returned %d: %s", resp.StatusCode, errBody.String())
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return utils.ChatMessage{}, err
	}

	debugResponse, _ := json.MarshalIndent(result, "", "\t")
	println("Got response:", string(debugResponse))

	if len(result.Choices) == 0 {
		return utils.ChatMessage{}, fmt.Errorf("no choices returned in response")
	}

	return result.Choices[0].Message, nil
}

func (o *OpenRouter) Chat(ctx context.Context, systemPrompt string, model string, onMessage func(string)) (string, error) {
	messages := []utils.ChatMessage{
		{
			Role:    "system",
			Content: systemPrompt + "\n\nYou have access to tools: execute_command (run bash commands), and web_search. Use them to accomplish the task. When you are done, call end_task (omit the reason parameter for success). If something goes wrong or you cannot complete the objective, call end_task with reason=\"explain why\" to signal failure.\n\nIMPORTANT: User input is not set up for tasks yet. If something goes wrong or you need information you don't have, call end_task to stop rather than waiting for user input.",
		},
	}

	const maxIterations = 200
	for i := 0; i < maxIterations; i++ {
		allowFallbacks := true

		useModel := o.model
		if model != "" {
			useModel = model
		}
		reqBody := chatRequest{
			Model:    useModel,
			Messages: messages,
			Reasoning: &reasoningConfig{
				Effort: "none",
			},
			MaxTokens: 10_000,
			Tools: []Tool{
				TerminalTool,
				EndTaskTool,
				{
					Type: "openrouter:web_search",
				},
			},
			Provider: &providerConfig{
				Order:          []string{"novita"},
				AllowFallbacks: &allowFallbacks,
			},
		}

		responseMsg, err := o.rawChat(ctx, reqBody)
		if err != nil {
			return "", err
		}

		messages = append(messages, responseMsg)

		// Send intermediate text to user if present
		if responseMsg.Content != "" {
			if contentStr, ok := responseMsg.Content.(string); ok {
				if onMessage != nil {
					onMessage(contentStr)
				}
			}
		}

		if len(responseMsg.ToolCalls) == 0 {
			content, ok := responseMsg.Content.(string)
			if !ok {
				return "", fmt.Errorf("unexpected non-string content type")
			}
			return content, nil
		}

		for _, toolCall := range responseMsg.ToolCalls {
			switch toolCall.Function.Name {
			case "execute_command":
				var args ExecuteCommandArgs
				if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
					return "", fmt.Errorf("failed to parse tool arguments: %v", err)
				}

				print("Executing command:", args.Command)
				output, err := ExecuteCommand(args.Command)
				if err != nil {
					output = fmt.Sprintf("Error: %v\nOutput: %s", err, output)
				}
				//println("Output:", output)

				message := utils.ChatMessage{
					Role:       "tool",
					Name:       "execute_command",
					ToolCallID: toolCall.ID,
					Content:    output,
				}
				messages = append(messages, message)

			case "append_notes":
				var args AppendNotesArgs
				if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
					return "", fmt.Errorf("failed to parse tool arguments: %v", err)
				}

				print("Appending to notes:", args.Content, "scope:", args.Scope)
				output, err := AppendNotes(args.Content, args.Scope, 0)
				if err != nil {
					output = fmt.Sprintf("Error: %v", err)
				}
				println("Output:", output)

				message := utils.ChatMessage{
					Role:       "tool",
					Name:       "append_notes",
					ToolCallID: toolCall.ID,
					Content:    output,
				}
				messages = append(messages, message)

			case "end_task":
				var args EndTaskArgs
				if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
					return "", fmt.Errorf("failed to parse end_task arguments: %v", err)
				}
				if args.Reason != "" {
					return "", fmt.Errorf("task failed: %s", args.Reason)
				}
				message := utils.ChatMessage{
					Role:       "tool",
					Name:       "end_task",
					ToolCallID: toolCall.ID,
					Content:    "Task ended.",
				}
				messages = append(messages, message)

			default:
				message := utils.ChatMessage{
					Role:       "tool",
					Name:       toolCall.Function.Name,
					ToolCallID: toolCall.ID,
					Content:    fmt.Sprintf("Error: Unknown tool %s", toolCall.Function.Name),
				}
				messages = append(messages, message)
			}
		}
	}

	return "", fmt.Errorf("exceeded max tool call iterations")
}
func (o *OpenRouter) ChatJSON(ctx context.Context, systemPrompt string) (string, error) {
	messages := []utils.ChatMessage{
		{
			Role:    "system",
			Content: systemPrompt,
		},
	}

	reqBody := chatRequest{
		Model:    o.model,
		Messages: messages,
		Reasoning: &reasoningConfig{
			Effort: "none",
		},
		ResponseFormat: &responseFormatConfig{
			Type: "json_object",
		},
		MaxTokens: 2_000,
	}

	responseMsg, err := o.rawChat(ctx, reqBody)
	if err != nil {
		return "", err
	}

	if contentStr, ok := responseMsg.Content.(string); ok {
		return contentStr, nil
	}
	return "", fmt.Errorf("unexpected response type")
}
