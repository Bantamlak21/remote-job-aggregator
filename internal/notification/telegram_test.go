package notification

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/httpclient"
)

func newTestTelegram(t *testing.T, handler http.HandlerFunc) (Telegram, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return Telegram{
		BotToken:   "test-token",
		HTTPClient: httpclient.New(httpclient.DefaultConfig()),
		BaseURL:    srv.URL,
	}, srv
}

func TestTelegram_SendPostsChatIDAndText(t *testing.T) {
	var gotPath string
	var gotBody struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	tg, _ := newTestTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	})

	if err := tg.Send(context.Background(), Message{ChatID: 42, Text: "new job for you"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/bottest-token/sendMessage" {
		t.Errorf("path = %q, want /bottest-token/sendMessage", gotPath)
	}
	if gotBody.ChatID != 42 || gotBody.Text != "new job for you" {
		t.Errorf("body = %+v, want chat_id=42 text=%q", gotBody, "new job for you")
	}
}

func TestTelegram_SendReturnsErrorOnAPIRefusal(t *testing.T) {
	tg, _ := newTestTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`))
	})

	err := tg.Send(context.Background(), Message{ChatID: 42, Text: "hi"})
	if err == nil {
		t.Fatal("Send: want an error when the API says ok:false, got nil")
	}
}

func TestTelegram_SendReturnsErrorOnTransportFailure(t *testing.T) {
	tg, srv := newTestTelegram(t, func(w http.ResponseWriter, r *http.Request) {})
	srv.Close() // the client now talks to a closed listener

	if err := tg.Send(context.Background(), Message{ChatID: 1, Text: "x"}); err == nil {
		t.Fatal("Send: want an error when the server is unreachable, got nil")
	}
}

func TestParseStartCommand(t *testing.T) {
	cases := []struct {
		text     string
		wantCode string
		wantOK   bool
	}{
		{"/start abc123", "abc123", true},
		{"/start@MyBot abc123", "abc123", true},
		{"/start", "", false},
		{"/start abc123 extra", "", false},
		{"hello there", "", false},
		{"", "", false},
		{"/help abc123", "", false},
	}
	for _, c := range cases {
		code, ok := parseStartCommand(c.text)
		if code != c.wantCode || ok != c.wantOK {
			t.Errorf("parseStartCommand(%q) = (%q, %v), want (%q, %v)", c.text, code, ok, c.wantCode, c.wantOK)
		}
	}
}

func TestListener_ConfirmsLinkFromStartCommandAndAdvancesOffset(t *testing.T) {
	type call struct {
		code   string
		chatID int64
	}
	var mu sync.Mutex
	var calls []call
	confirmer := confirmerFunc(func(_ context.Context, code string, chatID int64) error {
		mu.Lock()
		calls = append(calls, call{code, chatID})
		mu.Unlock()
		return nil
	})

	var requestOffsets []string
	served := false
	tg, srv := newTestTelegram(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestOffsets = append(requestOffsets, r.URL.Query().Get("offset"))
		first := !served
		served = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			_, _ = w.Write([]byte(`{"ok":true,"result":[
				{"update_id":100,"message":{"chat":{"id":555},"text":"/start linkcode1"}},
				{"update_id":101,"message":{"chat":{"id":777},"text":"hello"}}
			]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	})
	_ = srv

	l := NewListener(tg, confirmer, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()

	// Wait for the *second* poll request, not just the confirm call: the
	// confirm happens while handling the first response, before the loop
	// starts its second poll -- canceling as soon as calls==1 races the
	// listener's own next iteration and can cancel before it ever issues
	// that second request, which is what advances (and proves) the offset.
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(requestOffsets) >= 2 })
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0].code != "linkcode1" || calls[0].chatID != 555 {
		t.Fatalf("calls = %+v, want exactly one confirm for code=linkcode1 chatID=555", calls)
	}
	if len(requestOffsets) < 2 || requestOffsets[0] != "0" || requestOffsets[1] != "102" {
		t.Fatalf("requestOffsets = %v, want first \"0\" then \"102\" (update_id 101 + 1)", requestOffsets)
	}
}

type confirmerFunc func(ctx context.Context, code string, chatID int64) error

func (f confirmerFunc) ConfirmTelegramLink(ctx context.Context, code string, chatID int64) error {
	return f(ctx, code, chatID)
}
