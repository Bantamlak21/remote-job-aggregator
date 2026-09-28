package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pollSeconds is the Bot API's own long-poll wait, server side: one
// getUpdates call blocks up to this long waiting for a new update before
// answering with an empty result. pollHTTPGrace pads the HTTP client's own
// timeout past that, so a slow-but-still-within-budget poll is never cut
// off by the transport before Telegram itself would have answered.
const (
	pollSeconds    = 25
	pollHTTPGrace  = 10 * time.Second
	pollRetryDelay = 5 * time.Second
)

// LinkConfirmer maps a Telegram link code back to the account that
// requested it. internal/account.Service implements this.
type LinkConfirmer interface {
	ConfirmTelegramLink(ctx context.Context, code string, chatID int64) error
}

// update is the slice of one Telegram update this package cares about: a
// direct message's chat and text. Every other field the Bot API sends is
// ignored.
type update struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

type getUpdatesResponse struct {
	telegramResponse
	Result []update `json:"result"`
}

// Listener long-polls getUpdates and confirms any "/start <code>" message
// it sees -- the deep link (https://t.me/<bot>?start=<code>) a person opens
// after requesting a link on the web sends exactly that. No webhook server:
// long-polling needs no public endpoint or TLS certificate, and this
// project runs one instance (CLAUDE.md: no distributed scheduling infra),
// so there is nothing a webhook would need to load-balance across.
type Listener struct {
	Telegram  Telegram
	Confirmer LinkConfirmer
	Logger    *slog.Logger

	pollClient *http.Client
}

// NewListener returns a Listener ready for Run.
func NewListener(tg Telegram, confirmer LinkConfirmer, logger *slog.Logger) *Listener {
	return &Listener{
		Telegram:   tg,
		Confirmer:  confirmer,
		Logger:     logger,
		pollClient: &http.Client{Timeout: pollSeconds*time.Second + pollHTTPGrace},
	}
}

// Run polls until ctx is done, then returns. One goroutine, one clear
// shutdown path: the in-flight request is bound to ctx, so cancellation
// unblocks it directly rather than waiting for the next poll to start.
func (l *Listener) Run(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		updates, next, err := l.poll(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			l.Logger.Error("polling telegram for updates failed", "error", err)
			if !sleepCtx(ctx, pollRetryDelay) {
				return
			}
			continue
		}
		offset = next
		for _, u := range updates {
			code, ok := parseStartCommand(u.Message.Text)
			if !ok {
				continue
			}
			if err := l.Confirmer.ConfirmTelegramLink(ctx, code, u.Message.Chat.ID); err != nil {
				l.Logger.Warn("confirming a telegram link failed", "error", err)
			}
		}
	}
}

func (l *Listener) poll(ctx context.Context, offset int64) ([]update, int64, error) {
	endpoint, err := url.Parse(l.Telegram.endpoint("getUpdates"))
	if err != nil {
		return nil, offset, fmt.Errorf("notification: parsing getUpdates endpoint: %w", err)
	}
	q := endpoint.Query()
	q.Set("offset", strconv.FormatInt(offset, 10))
	q.Set("timeout", strconv.Itoa(pollSeconds))
	q.Set("allowed_updates", `["message"]`)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, offset, fmt.Errorf("notification: building getUpdates request: %w", err)
	}
	resp, err := l.pollClient.Do(req)
	if err != nil {
		return nil, offset, fmt.Errorf("notification: getUpdates: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out getUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, offset, fmt.Errorf("notification: decoding getUpdates response: %w", err)
	}
	if !out.OK {
		return nil, offset, fmt.Errorf("notification: getUpdates refused: %s (code %d)", out.Description, out.ErrorCode)
	}

	next := offset
	for _, u := range out.Result {
		if u.UpdateID+1 > next {
			next = u.UpdateID + 1
		}
	}
	return out.Result, next, nil
}

// parseStartCommand reports the code in a "/start <code>" message
// ("/start@BotName <code>" too, the form Telegram uses in group chats).
func parseStartCommand(text string) (code string, ok bool) {
	fields := strings.Fields(text)
	if len(fields) != 2 {
		return "", false
	}
	cmd := fields[0]
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	if cmd != "/start" {
		return "", false
	}
	return fields[1], true
}

// sleepCtx waits for d or ctx's cancellation, whichever comes first,
// reporting which happened.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
