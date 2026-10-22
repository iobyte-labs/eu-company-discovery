// Package llm is the engine's optional, additive use of a language model.
//
// Read this before using it anywhere: there is exactly one kind of question a model may be
// asked here, and it is not a question of fact.
//
// A model may answer a judgement — "does this company work in pharmaceuticals, or does it
// sell services to companies that do?" — where being wrong costs one wasted visit and a
// human can see the answer and disagree with it.
//
// A model may never answer a question of fact: what a company's address is, what its name
// is, where its careers page lives. Those are read off a page or they do not exist, and
// [model.VerificationPublished] means "this appeared, in full, on a page we fetched". An
// answer a model composed cannot carry that claim, and the dataset's worth rests on it.
//
// Everything here is optional. With no API key configured there is no client, callers
// continue, and nothing is written. That is [errors.KindProvider]'s whole purpose: an
// optional provider failing is not a failure of the run.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/iobyte-labs/eu-company-discovery/pkg/fetch"
)

// Defaults. The endpoint is OpenRouter's, which speaks the widely-implemented
// chat-completions shape, so any compatible host — including a local one — works by
// setting EUCD_LLM_BASE_URL.
const (
	DefaultBaseURL = "https://openrouter.ai/api/v1"
	DefaultTimeout = 60 * time.Second
	// MaxContentBytes bounds what is sent. A page is untrusted input and a long one is
	// mostly navigation; sending more costs money and adds nothing.
	MaxContentBytes = 12000
)

// Options configures a [Client].
type Options struct {
	// BaseURL is the chat-completions host. Empty uses DefaultBaseURL.
	BaseURL string
	// APIKey is read from the environment by [FromEnv]; empty means no client at all.
	APIKey string
	// Model is the provider's model id. There is deliberately no default: a model name
	// hardcoded here rots silently when a provider retires it, and the engine would rather
	// say "no model configured" than quietly ask for one that no longer exists.
	Model string
	// Timeout bounds one request. Empty uses DefaultTimeout.
	Timeout time.Duration
	// Guard dials the connection. nil builds the engine's default, which refuses a base
	// URL pointing anywhere but the public internet — worth having, because this host is
	// configurable and a mistyped one should not reach a metadata endpoint.
	Guard *fetch.Guard
}

// Client talks to a chat-completions endpoint. A nil *Client is usable and answers nothing,
// which is what "no key configured" looks like to a caller.
type Client struct {
	http    *http.Client
	baseURL string
	apiKey  string
	model   string
}

// FromEnv builds a client from the environment, or nil when no key is set.
//
//	EUCD_LLM_API_KEY    the key. Without it there is no client.
//	EUCD_LLM_MODEL      the model id, e.g. "openai/gpt-4o-mini". Required.
//	EUCD_LLM_BASE_URL   a chat-completions host. Optional.
//
// It returns (nil, nil) when the key is absent: not configured is not an error.
func FromEnv() (*Client, error) {
	key := strings.TrimSpace(os.Getenv("EUCD_LLM_API_KEY"))
	if key == "" {
		return nil, nil
	}
	return New(Options{
		APIKey:  key,
		Model:   strings.TrimSpace(os.Getenv("EUCD_LLM_MODEL")),
		BaseURL: strings.TrimSpace(os.Getenv("EUCD_LLM_BASE_URL")),
	})
}

// New builds a client. It returns (nil, nil) when no key is given.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, nil
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, fmt.Errorf("a model id is required (EUCD_LLM_MODEL), " +
			"because a default hardcoded here would rot when the provider retires it")
	}
	guard := opts.Guard
	if guard == nil {
		guard = fetch.NewGuard()
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	// The base URL is configuration, which makes it input. Checking it through the same
	// guard the crawler uses means a mistyped host cannot reach the private network.
	if _, err := guard.CheckURL(baseURL); err != nil {
		return nil, fmt.Errorf("llm base url: %w", err)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{DialContext: guard.DialContext, Proxy: nil},
			Timeout:   timeout,
		},
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  opts.APIKey,
		model:   opts.Model,
	}, nil
}

// Available reports whether there is a model to ask. A nil client is not available, which
// is how "no key" reaches a caller without a special case.
func (c *Client) Available() bool { return c != nil }

// Model is the configured model id, for recording alongside whatever it answered.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    float64       `json:"temperature"`
	MaxTokens      int           `json:"max_tokens"`
	ResponseFormat *format       `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type format struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// AskJSON sends instructions and content, and decodes the reply into out.
//
// instructions come from this repository and nowhere else. content is untrusted — it is
// page text — and is passed as material to judge, inside its own delimiters, never as
// something that can change what was asked. A page reading "ignore previous instructions"
// is a string in a document, and the decode below is what decides whether the answer is
// usable: a reply that does not fit the caller's type is discarded, whatever it says.
func (c *Client) AskJSON(ctx context.Context, instructions, content string, out any) error {
	if c == nil {
		return fmt.Errorf("no model configured")
	}
	if len(content) > MaxContentBytes {
		content = content[:MaxContentBytes]
	}

	body, err := json.Marshal(chatRequest{
		Model:          c.model,
		Temperature:    0, // the same page should get the same judgement twice
		MaxTokens:      400,
		ResponseFormat: &format{Type: "json_object"},
		Messages: []chatMessage{
			{Role: "system", Content: instructions},
			{Role: "user", Content: "<document>\n" + content + "\n</document>"},
		},
	})
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	// OpenRouter asks callers to identify themselves; it is also simply polite.
	request.Header.Set("HTTP-Referer", fetch.ContactURL)
	request.Header.Set("X-Title", "eu-company-discovery")

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("ask the model: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // nothing useful to do about it

	var reply chatResponse
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		return fmt.Errorf("the model's reply was not JSON: %w", err)
	}
	if reply.Error != nil && reply.Error.Message != "" {
		return fmt.Errorf("the model refused: %s", reply.Error.Message)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("the model returned HTTP %d", response.StatusCode)
	}
	if len(reply.Choices) == 0 {
		return fmt.Errorf("the model returned no answer")
	}

	answer := strings.TrimSpace(reply.Choices[0].Message.Content)
	// Some models wrap JSON in a fenced block however firmly they are asked not to.
	answer = strings.TrimPrefix(answer, "```json")
	answer = strings.TrimPrefix(answer, "```")
	answer = strings.TrimSuffix(strings.TrimSpace(answer), "```")

	// Decoded into a fresh value first, and copied to the caller only once the whole reply
	// has proved to be the right shape. encoding/json fills fields as it goes and errors
	// on the unexpected one afterwards, so decoding straight into out would leave a
	// refused answer half-written where a careless caller could still read it.
	target := reflect.ValueOf(out)
	if target.Kind() != reflect.Pointer || target.IsNil() {
		return fmt.Errorf("the answer must be decoded into a non-nil pointer")
	}
	fresh := reflect.New(target.Elem().Type())

	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(answer)))
	// An unexpected field means the reply is not the shape that was asked for, and a reply
	// of the wrong shape is not evidence of anything.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(fresh.Interface()); err != nil {
		return fmt.Errorf("the model's answer did not fit the expected shape: %w", err)
	}
	target.Elem().Set(fresh.Elem())
	return nil
}
