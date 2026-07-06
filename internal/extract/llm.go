// Package extract implements the ingest-time card/fact extractor of spec
// §10.4: an LLM reads newly ingested evidence and proposes typed candidates
// with MANDATORY verbatim citations — anything it cannot cite is discarded,
// not stored (I2). All outputs enter the normal proposal → promotion gate
// (§10.6); nothing extracted here reaches `active` directly.
package extract

import (
	"context"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/lazorfuzz/memba/internal/config"
)

// LLM is the minimal completion surface the extractor needs. Anthropic is
// the shipping adapter; tests use a fake.
type LLM interface {
	Complete(ctx context.Context, system, user string) (string, error)
	ModelID() string
}

// NewLLM builds the configured extractor model client. Returns (nil, nil)
// when extraction is disabled (provider "none"/"" or missing credentials) —
// the worker then records extract_cards jobs as no-ops.
func NewLLM(m config.ModelRef) (LLM, error) {
	switch m.Provider {
	case "", "none", "disabled":
		return nil, nil
	case "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
			// No credentials: extraction stays off rather than failing every job.
			return nil, nil
		}
		return newAnthropicLLM(m.Model), nil
	default:
		return nil, fmt.Errorf("unknown extractor provider %q", m.Provider)
	}
}

// resolveModel maps the spec's config alias to a concrete model ID.
func resolveModel(model string) anthropic.Model {
	switch model {
	case "", "claude-sonnet-latest":
		return anthropic.Model("claude-sonnet-5")
	case "claude-opus-latest":
		return anthropic.Model("claude-opus-4-8")
	default:
		return anthropic.Model(model)
	}
}

type anthropicLLM struct {
	client anthropic.Client
	model  anthropic.Model
}

func newAnthropicLLM(model string) *anthropicLLM {
	return &anthropicLLM{client: anthropic.NewClient(), model: resolveModel(model)}
}

func (a *anthropicLLM) ModelID() string { return "anthropic/" + string(a.model) }

func (a *anthropicLLM) Complete(ctx context.Context, system, user string) (string, error) {
	// Streaming + accumulate: evidence bodies can be large and extraction
	// outputs long enough that streaming avoids request timeouts.
	stream := a.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     a.model,
		MaxTokens: 8192,
		System: []anthropic.TextBlockParam{{
			Text:         system,
			CacheControl: anthropic.NewCacheControlEphemeralParam(), // stable prompt prefix across jobs
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	message := anthropic.Message{}
	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			return "", err
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	if message.StopReason == anthropic.StopReasonRefusal {
		return "", fmt.Errorf("extractor model refused the request")
	}
	var out string
	for _, block := range message.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			out += tb.Text
		}
	}
	return out, nil
}
