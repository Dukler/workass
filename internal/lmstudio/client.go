package lmstudio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	BaseURL    string
	Model      string
	APIKey     string
	HTTPClient *http.Client
}

func ConfigFromEnv() Config {
	return Config{
		BaseURL: strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")),
		Model:   strings.TrimSpace(os.Getenv("OPENAI_MODEL")),
		APIKey:  os.Getenv("OPENAI_API_KEY"),
	}
}

type Client struct {
	baseURL *url.URL
	model   string
	apiKey  string
	http    *http.Client
}

func New(cfg Config) (*Client, error) {
	var base *url.URL
	if strings.TrimSpace(cfg.BaseURL) != "" {
		parsed, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
		if err != nil {
			return nil, fmt.Errorf("invalid OPENAI_BASE_URL: %w", err)
		}
		if parsed.Scheme == "" || parsed.Host == "" {
			return nil, errors.New("OPENAI_BASE_URL must include scheme and host")
		}
		base = parsed
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL: base,
		model:   strings.TrimSpace(cfg.Model),
		apiKey:  cfg.APIKey,
		http:    httpClient,
	}, nil
}

func (c *Client) DefaultModel() string {
	if c == nil {
		return ""
	}
	return c.model
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object,omitempty"`
	OwnedBy string `json:"owned_by,omitempty"`
}

type modelsResponse struct {
	Data []Model `json:"data"`
}

func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	if c == nil {
		return nil, errors.New("OpenAI client is nil")
	}
	endpoint, err := c.endpoint("models")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, httpStatusError("models", resp)
	}
	var out modelsResponse
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	models := make([]Model, 0, len(out.Data))
	for _, model := range out.Data {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID != "" {
			models = append(models, model)
		}
	}
	return models, nil
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string
	Messages []Message
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type StreamEvent struct {
	Content      string
	Usage        *Usage
	FinishReason string
}

func (c *Client) StreamChat(ctx context.Context, chat ChatRequest, onEvent func(StreamEvent) error) error {
	if c == nil {
		return errors.New("OpenAI client is nil")
	}
	model := strings.TrimSpace(chat.Model)
	if model == "" {
		model = c.model
	}
	if model == "" {
		return errors.New("OPENAI_MODEL is required")
	}
	endpoint, err := c.endpoint("chat/completions")
	if err != nil {
		return err
	}
	body := struct {
		Model    string    `json:"model"`
		Messages []Message `json:"messages"`
		Stream   bool      `json:"stream"`
	}{
		Model:    model,
		Messages: chat.Messages,
		Stream:   true,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpStatusError("chat completions", resp)
	}
	err = readSSE(resp.Body, func(data string) error {
		if strings.TrimSpace(data) == "[DONE]" {
			return errDone
		}
		var chunk chatCompletionChunk
		dec := json.NewDecoder(strings.NewReader(data))
		if err := dec.Decode(&chunk); err != nil {
			return fmt.Errorf("decode chat completion stream: %w", err)
		}
		if chunk.Usage != nil {
			if err := onEvent(StreamEvent{Usage: chunk.Usage}); err != nil {
				return err
			}
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := onEvent(StreamEvent{Content: choice.Delta.Content}); err != nil {
					return err
				}
			}
			if choice.FinishReason != "" {
				if err := onEvent(StreamEvent{FinishReason: choice.FinishReason}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return nil
}

type chatCompletionChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

var errDone = errors.New("sse done")

func readSSE(r io.Reader, onData func(string) error) error {
	br := bufio.NewReader(r)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		data = nil
		err := onData(joined)
		if errors.Is(err, errDone) {
			return errDone
		}
		return err
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if err := flush(); err != nil {
					if errors.Is(err, errDone) {
						return nil
					}
					return err
				}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "data:"):
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if flushErr := flush(); flushErr != nil && !errors.Is(flushErr, errDone) {
					return flushErr
				}
				return nil
			}
			return err
		}
	}
}

func (c *Client) endpoint(path string) (string, error) {
	if c.baseURL == nil {
		return "", errors.New("OPENAI_BASE_URL is required")
	}
	u := *c.baseURL
	basePath := strings.TrimRight(u.Path, "/")
	if basePath == "" {
		basePath = "/v1"
	}
	u.Path = basePath + "/" + strings.TrimLeft(path, "/")
	return u.String(), nil
}

func (c *Client) authorize(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

func httpStatusError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("%s failed: HTTP %d", op, resp.StatusCode)
	}
	return fmt.Errorf("%s failed: HTTP %d: %s", op, resp.StatusCode, msg)
}
