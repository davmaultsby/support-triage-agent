package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Ollama runs the agent on an open-weights model served by a local Ollama,
// through Ollama's native chat API.
type Ollama struct {
	URL   string // for example http://localhost:11434
	Model string
}

func (o *Ollama) Name() string { return "ollama:" + o.Model }

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func (o *Ollama) Start(system, user string, tools []ToolSpec) Conversation {
	defs := make([]map[string]any, len(tools))
	for i, s := range tools {
		defs[i] = map[string]any{"type": "function", "function": map[string]any{
			"name": s.Name, "description": s.Description,
			"parameters": map[string]any{"type": "object", "properties": s.Properties, "required": s.required()},
		}}
	}
	return &ollamaConversation{o: o, tools: defs, messages: []ollamaMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: user},
	}}
}

type ollamaConversation struct {
	o        *Ollama
	tools    []map[string]any
	messages []ollamaMessage
	turn     int
}

func (cv *ollamaConversation) Next(ctx context.Context, results []ToolResult, note string) (Turn, error) {
	for _, r := range results {
		content := r.Content
		if r.IsError {
			content = "error: " + content
		}
		cv.messages = append(cv.messages, ollamaMessage{Role: "tool", Content: content, ToolName: r.Name})
	}
	if note != "" {
		cv.messages = append(cv.messages, ollamaMessage{Role: "user", Content: note})
	}
	body, _ := json.Marshal(map[string]any{
		"model": cv.o.Model, "messages": cv.messages, "tools": cv.tools, "stream": false,
		"options": map[string]any{"temperature": 0, "num_ctx": 16384},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cv.o.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Turn{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Turn{}, err
	}
	defer resp.Body.Close()
	var res struct {
		Message         ollamaMessage `json:"message"`
		PromptEvalCount int64         `json:"prompt_eval_count"`
		EvalCount       int64         `json:"eval_count"`
		Error           string        `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return Turn{}, fmt.Errorf("ollama: %s: %w", resp.Status, err)
	}
	if res.Error != "" {
		return Turn{}, fmt.Errorf("ollama: %s", res.Error)
	}
	cv.messages = append(cv.messages, res.Message)
	cv.turn++

	t := Turn{Text: res.Message.Content, Usage: Usage{InputTokens: res.PromptEvalCount, OutputTokens: res.EvalCount}}
	for i, c := range res.Message.ToolCalls {
		// Ollama does not assign call IDs; results are matched by order.
		t.Calls = append(t.Calls, ToolCall{ID: fmt.Sprintf("call_%d_%d", cv.turn, i), Name: c.Function.Name, Input: c.Function.Arguments})
	}
	return t, nil
}
