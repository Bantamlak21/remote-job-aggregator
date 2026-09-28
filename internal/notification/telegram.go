package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

const defaultTelegramBaseURL = "https://api.telegram.org"

// Telegram sends messages through the Telegram Bot HTTP API
// (https://core.telegram.org/bots/api).
type Telegram struct {
	BotToken   string
	HTTPClient *httpclient.Client
	// BaseURL overrides api.telegram.org; empty means the real API. Tests
	// point it at an httptest.Server so nothing here ever talks to the
	// genuine Telegram service.
	BaseURL string
}

func (t Telegram) baseURL() string {
	if t.BaseURL != "" {
		return t.BaseURL
	}
	return defaultTelegramBaseURL
}

func (t Telegram) endpoint(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", t.baseURL(), t.BotToken, method)
}

// telegramResponse is the envelope every Bot API call replies with.
type telegramResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
}

// Send posts m to the Bot API's sendMessage endpoint.
func (t Telegram) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}{m.ChatID, m.Text})
	if err != nil {
		return fmt.Errorf("notification: encoding sendMessage: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint("sendMessage"), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notification: building sendMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("notification: sendMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out telegramResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("notification: decoding sendMessage response: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("notification: sendMessage refused: %s (code %d)", out.Description, out.ErrorCode)
	}
	return nil
}
