package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Bantamlak12/remote-job-aggregator/internal/account"
)

func TestNotificationVocabulary_ReturnsTheKeywordListWithoutSigningIn(t *testing.T) {
	e := newEnv(t, envOpts{})
	rec := e.do("GET", "/api/v1/notification-keywords", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/notification-keywords = %d, want 200", rec.Code)
	}
	got := decode[map[string][]string](t, rec)
	if len(got["keywords"]) == 0 {
		t.Fatal("keywords is empty")
	}
	found := false
	for _, k := range got["keywords"] {
		if k == "golang" {
			found = true
		}
	}
	if !found {
		t.Errorf("keywords = %v, want it to include %q", got["keywords"], "golang")
	}
}

func TestNotifications_RequireSignIn(t *testing.T) {
	e := newEnv(t, envOpts{})
	if rec := e.do("GET", "/api/v1/me/notifications", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET (signed out) = %d, want 401", rec.Code)
	}
	if rec := e.do("PUT", "/api/v1/me/notifications", `{"keywords":["golang"]}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("PUT (signed out) = %d, want 401", rec.Code)
	}
}

func TestNotifications_DefaultsEmptyThenSetAndGetRoundTrip(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("keywords@example.com")

	got := decode[map[string][]string](t, e.do("GET", "/api/v1/me/notifications", "", withCookie(c)))
	if len(got["keywords"]) != 0 {
		t.Fatalf("keywords before setting any = %v, want none", got["keywords"])
	}

	if rec := e.do("PUT", "/api/v1/me/notifications", `{"keywords":["golang","backend"]}`, withCookie(c)); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT = %d %q, want 204", rec.Code, rec.Body)
	}
	got = decode[map[string][]string](t, e.do("GET", "/api/v1/me/notifications", "", withCookie(c)))
	if strings.Join(got["keywords"], ",") != "backend,golang" {
		t.Errorf("keywords after PUT = %v, want [backend golang]", got["keywords"])
	}
}

func TestNotifications_PutRejectsAnUnknownKeyword(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("bad-keyword@example.com")
	rec := e.do("PUT", "/api/v1/me/notifications", `{"keywords":["not-a-real-keyword"]}`, withCookie(c))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT (unknown keyword) = %d %q, want 400", rec.Code, rec.Body)
	}
	body := decode[map[string]string](t, rec)
	if body["code"] != "unknown_keyword" {
		t.Errorf("code = %q, want %q", body["code"], "unknown_keyword")
	}
}

func TestTelegramLink_RefusedWhenNotConfigured(t *testing.T) {
	e := newEnv(t, envOpts{}) // no TelegramBotUsername
	c := e.signIn("notconfigured@example.com")
	rec := e.do("POST", "/api/v1/me/telegram/link", "", withCookie(c))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /me/telegram/link = %d %q, want 503", rec.Code, rec.Body)
	}
}

func TestTelegramLink_RequestConfirmAndUnlink(t *testing.T) {
	e := newEnv(t, envOpts{tune: func(c *account.Config) { c.TelegramBotUsername = "TestBot" }})
	c := e.signIn("telegram@example.com")

	if got := decode[map[string]bool](t, e.do("GET", "/api/v1/me/telegram/status", "", withCookie(c))); got["linked"] {
		t.Fatal("linked before any link request = true, want false")
	}

	rec := e.do("POST", "/api/v1/me/telegram/link", "", withCookie(c))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /me/telegram/link = %d %q, want 200", rec.Code, rec.Body)
	}
	body := decode[map[string]any](t, rec)
	deepLink, _ := body["deep_link"].(string)
	if !strings.HasPrefix(deepLink, "https://t.me/TestBot?start=") {
		t.Fatalf("deep_link = %q, want the https://t.me/TestBot?start=<code> shape", deepLink)
	}
	u, err := url.Parse(deepLink)
	if err != nil {
		t.Fatalf("parsing deep_link: %v", err)
	}
	code := u.Query().Get("start")
	// Unlike a login or session token, this code is meant to appear in this
	// very response: it's the deep link the signed-in person clicks, not
	// something delivered over a side channel -- so it is deliberately not
	// registered as a secret the transcript-leak check (addSecret) would flag.

	// Confirming is the listener's job in production (see internal/notification),
	// not an HTTP endpoint: simulate it by calling the service directly, exactly
	// as internal/notification.Listener would.
	if err := e.svc.ConfirmTelegramLink(context.Background(), code, 999); err != nil {
		t.Fatalf("ConfirmTelegramLink: %v", err)
	}

	if got := decode[map[string]bool](t, e.do("GET", "/api/v1/me/telegram/status", "", withCookie(c))); !got["linked"] {
		t.Fatal("linked after confirming = false, want true")
	}

	if rec := e.do("DELETE", "/api/v1/me/telegram/link", "", withCookie(c)); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /me/telegram/link = %d, want 204", rec.Code)
	}
	if got := decode[map[string]bool](t, e.do("GET", "/api/v1/me/telegram/status", "", withCookie(c))); got["linked"] {
		t.Fatal("linked after unlinking = true, want false")
	}
}

func TestTelegramLink_IsRateLimitedPerAccount(t *testing.T) {
	e := newEnv(t, envOpts{tune: func(c *account.Config) {
		c.TelegramBotUsername = "TestBot"
		c.TelegramLinkPerHour = 1
	}})
	c := e.signIn("ratelimited@example.com")

	if rec := e.do("POST", "/api/v1/me/telegram/link", "", withCookie(c)); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", rec.Code)
	}
	rec := e.do("POST", "/api/v1/me/telegram/link", "", withCookie(c))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d %q, want 429", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 response has no Retry-After header")
	}
}
