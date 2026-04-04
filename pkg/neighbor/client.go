package neighbor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Config struct {
	Name  string
	URL   string
	Token string
}

type Client struct {
	name   string
	url    string
	token  string
	client *http.Client
}

func NewClient(cfg Config) *Client {
	return &Client{
		name:   cfg.Name,
		url:    cfg.URL,
		token:  cfg.Token,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

type chatReq struct {
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
}

type chatResp struct {
	Response string `json:"response"`
}

func (c *Client) SendMessage(ctx context.Context, message string) (string, error) {
	body, err := json.Marshal(chatReq{
		SessionID: "neighbor:" + c.name,
		Prompt:    message,
	})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("neighbor %s returned %d: %s", c.name, resp.StatusCode, string(respBody))
	}

	var result chatResp
	if err = json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}

	return result.Response, nil
}
