package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// Claude runs the agent on a Claude model through the Anthropic API.
// The client reads ANTHROPIC_API_KEY from the environment.
type Claude struct {
	client anthropic.Client
	model  string
	effort anthropic.BetaOutputConfigEffort // empty keeps the model's default
}

func NewClaude(model, effort string) *Claude {
	return &Claude{client: anthropic.NewClient(), model: model, effort: anthropic.BetaOutputConfigEffort(effort)}
}

func (c *Claude) Name() string {
	if c.effort != "" {
		return c.model + "@" + string(c.effort)
	}
	return c.model
}

func (c *Claude) Start(system, user string, tools []ToolSpec) Conversation {
	params := anthropic.BetaMessageNewParams{
		Model:     c.model,
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Tools:     claudeTools(tools),
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
		// Caches the growing conversation between the turns of one ticket.
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		// If a safety classifier declines a turn, the API retries it on a
		// fallback model it picks, inside the same call.
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
	}
	if c.effort != "" {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: c.effort}
	}
	return &claudeConversation{client: &c.client, params: params}
}

func claudeTools(specs []ToolSpec) []anthropic.BetaToolUnionParam {
	out := make([]anthropic.BetaToolUnionParam, len(specs))
	for i, s := range specs {
		out[i] = anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        s.Name,
			Description: anthropic.String(s.Description),
			Strict:      anthropic.Bool(true), // inputs always match the schema
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties:  s.Properties,
				Required:    s.required(),
				ExtraFields: map[string]any{"additionalProperties": false},
			},
		}}
	}
	return out
}

type claudeConversation struct {
	client *anthropic.Client
	params anthropic.BetaMessageNewParams
}

func (cv *claudeConversation) Next(ctx context.Context, results []ToolResult, note string) (Turn, error) {
	var blocks []anthropic.BetaContentBlockParamUnion
	for _, r := range results {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(r.CallID, r.Content, r.IsError))
	}
	if note != "" {
		blocks = append(blocks, anthropic.NewBetaTextBlock(note))
	}
	if len(blocks) > 0 {
		cv.params.Messages = append(cv.params.Messages, anthropic.NewBetaUserMessage(blocks...))
	}
	resp, err := cv.client.Beta.Messages.New(ctx, cv.params)
	if err != nil {
		return Turn{}, err
	}
	// Append the whole response, thinking blocks included, so the history stays append-only.
	cv.params.Messages = append(cv.params.Messages, resp.ToParam())

	t := Turn{Usage: Usage{
		InputTokens:      resp.Usage.InputTokens,
		OutputTokens:     resp.Usage.OutputTokens,
		CacheReadTokens:  resp.Usage.CacheReadInputTokens,
		CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
	}}
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		t.Refused = true
		return t, nil
	case anthropic.BetaStopReasonMaxTokens:
		return t, fmt.Errorf("response hit max_tokens")
	}
	for _, b := range resp.Content {
		switch v := b.AsAny().(type) {
		case anthropic.BetaToolUseBlock:
			t.Calls = append(t.Calls, ToolCall{ID: v.ID, Name: v.Name, Input: json.RawMessage(v.JSON.Input.Raw())})
		case anthropic.BetaTextBlock:
			t.Text += v.Text
		}
	}
	return t, nil
}
