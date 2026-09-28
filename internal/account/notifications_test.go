package account

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/relevance"
	"github.com/Bantamlak12/remote-job-aggregator/internal/mailer"
	"github.com/Bantamlak12/remote-job-aggregator/internal/notification"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(t *testing.T, pool *pgxpool.Pool, cfg Config) *Service {
	t.Helper()
	return NewService(NewStore(pool), mailer.Log{Logger: testLogger()}, cfg, testLogger())
}

// codeFromDeepLink extracts the "start" query parameter from a
// https://t.me/<bot>?start=<code> deep link.
func codeFromDeepLink(t *testing.T, deepLink string) string {
	t.Helper()
	u, err := url.Parse(deepLink)
	if err != nil {
		t.Fatalf("parsing deep link %q: %v", deepLink, err)
	}
	code := u.Query().Get("start")
	if code == "" {
		t.Fatalf("deep link %q has no start parameter", deepLink)
	}
	return code
}

func TestStoreSetNotificationKeywords_ReplacesWholesale(t *testing.T) {
	pool := newPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, store, "keywords@example.com")

	if err := store.SetNotificationKeywords(ctx, u.ID, []string{"golang", "backend"}); err != nil {
		t.Fatalf("SetNotificationKeywords: %v", err)
	}
	got, err := store.NotificationKeywords(ctx, u.ID)
	if err != nil {
		t.Fatalf("NotificationKeywords: %v", err)
	}
	if strings.Join(got, ",") != "backend,golang" {
		t.Errorf("NotificationKeywords = %v, want [backend golang]", got)
	}

	// A second call replaces, not appends.
	if err := store.SetNotificationKeywords(ctx, u.ID, []string{"python"}); err != nil {
		t.Fatalf("SetNotificationKeywords (replace): %v", err)
	}
	got, err = store.NotificationKeywords(ctx, u.ID)
	if err != nil {
		t.Fatalf("NotificationKeywords: %v", err)
	}
	if strings.Join(got, ",") != "python" {
		t.Errorf("NotificationKeywords after replace = %v, want [python]", got)
	}

	// Clearing with an empty slice leaves none.
	if err := store.SetNotificationKeywords(ctx, u.ID, []string{}); err != nil {
		t.Fatalf("SetNotificationKeywords (clear): %v", err)
	}
	got, err = store.NotificationKeywords(ctx, u.ID)
	if err != nil {
		t.Fatalf("NotificationKeywords: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("NotificationKeywords after clear = %v, want none", got)
	}
}

func TestStoreConfirmTelegramLink_LinksTheRequestingAccountAndSpendsTheToken(t *testing.T) {
	pool := newPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, store, "telegram@example.com")

	token, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTelegramLinkToken(ctx, u.ID, hash, 10*time.Minute); err != nil {
		t.Fatalf("CreateTelegramLinkToken: %v", err)
	}

	gotUserID, err := store.ConfirmTelegramLink(ctx, hashToken(token), 555)
	if err != nil {
		t.Fatalf("ConfirmTelegramLink: %v", err)
	}
	if gotUserID != u.ID {
		t.Errorf("ConfirmTelegramLink returned user %d, want %d", gotUserID, u.ID)
	}

	chatID, linked, err := store.TelegramChatID(ctx, u.ID)
	if err != nil {
		t.Fatalf("TelegramChatID: %v", err)
	}
	if !linked || chatID != 555 {
		t.Errorf("TelegramChatID = (%d, %v), want (555, true)", chatID, linked)
	}

	// The token is single-use.
	if _, err := store.ConfirmTelegramLink(ctx, hashToken(token), 555); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ConfirmTelegramLink (reused token) = %v, want ErrInvalidToken", err)
	}
}

func TestStoreConfirmTelegramLink_RefusesUnknownOrExpiredToken(t *testing.T) {
	pool := newPool(t)
	store := NewStore(pool)
	ctx := context.Background()

	if _, err := store.ConfirmTelegramLink(ctx, hashToken("nonexistent"), 1); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ConfirmTelegramLink (unknown) = %v, want ErrInvalidToken", err)
	}

	u, _, _ := mustLogin(t, store, "expired@example.com")
	token, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	// A whole millisecond survives postgres's microsecond rounding (a 1ns TTL
	// would round expires_at down to equal created_at and fail the table's own
	// "expires_at > created_at" CHECK before this test ever gets to exercise
	// ConfirmTelegramLink's handling of an actually-expired row).
	if err := store.CreateTelegramLinkToken(ctx, u.ID, hash, 1*time.Millisecond); err != nil {
		t.Fatalf("CreateTelegramLinkToken: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := store.ConfirmTelegramLink(ctx, hashToken(token), 1); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ConfirmTelegramLink (expired) = %v, want ErrInvalidToken", err)
	}
}

func TestStoreConfirmTelegramLink_RefusesAChatAlreadyLinkedToAnotherAccount(t *testing.T) {
	pool := newPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	alice, _, _ := mustLogin(t, store, "alice@example.com")
	bob, _, _ := mustLogin(t, store, "bob@example.com")

	_, aliceHash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTelegramLinkToken(ctx, alice.ID, aliceHash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmTelegramLink(ctx, aliceHash, 999); err != nil {
		t.Fatalf("linking alice: %v", err)
	}

	_, bobHash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTelegramLinkToken(ctx, bob.ID, bobHash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmTelegramLink(ctx, bobHash, 999); !errors.Is(err, ErrTelegramChatAlreadyLinked) {
		t.Errorf("linking bob to alice's chat = %v, want ErrTelegramChatAlreadyLinked", err)
	}
}

func TestStoreUnlinkTelegram_IsIdempotent(t *testing.T) {
	pool := newPool(t)
	store := NewStore(pool)
	ctx := context.Background()
	u, _, _ := mustLogin(t, store, "unlink@example.com")

	_, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTelegramLinkToken(ctx, u.ID, hash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmTelegramLink(ctx, hash, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.UnlinkTelegram(ctx, u.ID); err != nil {
		t.Fatalf("UnlinkTelegram: %v", err)
	}
	if err := store.UnlinkTelegram(ctx, u.ID); err != nil {
		t.Fatalf("UnlinkTelegram (again): %v", err)
	}
	if _, linked, err := store.TelegramChatID(ctx, u.ID); err != nil || linked {
		t.Errorf("TelegramChatID after unlink = (linked=%v, err=%v), want (false, nil)", linked, err)
	}
}

func TestServiceSetNotificationKeywords_RejectsUnknownKeywordAndTooMany(t *testing.T) {
	pool := newPool(t)
	svc := newTestService(t, pool, Config{})
	ctx := context.Background()
	u, _, _ := mustLogin(t, svc.store, "svc-keywords@example.com")

	if err := svc.SetNotificationKeywords(ctx, u.ID, []string{"not-a-real-keyword"}); !errors.Is(err, ErrUnknownKeyword) {
		t.Errorf("SetNotificationKeywords (unknown) = %v, want ErrUnknownKeyword", err)
	}

	vocab := relevance.RelevantVocabulary()
	if len(vocab) < 21 {
		t.Fatalf("test needs at least 21 known keywords, RelevantVocabulary has %d", len(vocab))
	}
	if err := svc.SetNotificationKeywords(ctx, u.ID, vocab[:21]); !errors.Is(err, ErrTooManyKeywords) {
		t.Errorf("SetNotificationKeywords (too many) = %v, want ErrTooManyKeywords", err)
	}

	if err := svc.SetNotificationKeywords(ctx, u.ID, []string{"golang", "golang"}); err != nil {
		t.Fatalf("SetNotificationKeywords (valid, with a duplicate): %v", err)
	}
	got, err := svc.NotificationKeywords(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "golang" {
		t.Errorf("NotificationKeywords = %v, want exactly [golang] (deduplicated)", got)
	}
}

func TestServiceRequestTelegramLink_RefusesWithoutBotUsernameConfigured(t *testing.T) {
	pool := newPool(t)
	svc := newTestService(t, pool, Config{}) // TelegramBotUsername left empty
	u, _, _ := mustLogin(t, svc.store, "notconfigured@example.com")

	if _, err := svc.RequestTelegramLink(context.Background(), u.ID); !errors.Is(err, ErrNotificationsNotConfigured) {
		t.Errorf("RequestTelegramLink = %v, want ErrNotificationsNotConfigured", err)
	}
}

func TestServiceRequestTelegramLink_IsRateLimitedPerAccount(t *testing.T) {
	pool := newPool(t)
	cfg := Config{TelegramBotUsername: "TestBot", TelegramLinkPerHour: 2}
	svc := newTestService(t, pool, cfg)
	ctx := context.Background()
	u, _, _ := mustLogin(t, svc.store, "ratelimit@example.com")

	for i := 0; i < 2; i++ {
		if _, err := svc.RequestTelegramLink(ctx, u.ID); err != nil {
			t.Fatalf("RequestTelegramLink #%d: %v", i, err)
		}
	}
	_, err := svc.RequestTelegramLink(ctx, u.ID)
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("RequestTelegramLink #3 = %v, want *RateLimitedError", err)
	}
}

func TestServiceRequestAndConfirmTelegramLink_EndToEnd(t *testing.T) {
	pool := newPool(t)
	cfg := Config{TelegramBotUsername: "TestBot"}
	svc := newTestService(t, pool, cfg)
	ctx := context.Background()
	u, _, _ := mustLogin(t, svc.store, "e2e@example.com")

	req, err := svc.RequestTelegramLink(ctx, u.ID)
	if err != nil {
		t.Fatalf("RequestTelegramLink: %v", err)
	}
	if !strings.HasPrefix(req.DeepLink, "https://t.me/TestBot?start=") {
		t.Errorf("DeepLink = %q, want the https://t.me/TestBot?start=<code> shape", req.DeepLink)
	}
	code := codeFromDeepLink(t, req.DeepLink)

	if linked, err := svc.TelegramLinked(ctx, u.ID); err != nil || linked {
		t.Fatalf("TelegramLinked before confirming = (%v, %v), want (false, nil)", linked, err)
	}
	if err := svc.ConfirmTelegramLink(ctx, code, 42); err != nil {
		t.Fatalf("ConfirmTelegramLink: %v", err)
	}
	if linked, err := svc.TelegramLinked(ctx, u.ID); err != nil || !linked {
		t.Fatalf("TelegramLinked after confirming = (%v, %v), want (true, nil)", linked, err)
	}

	if err := svc.ConfirmTelegramLink(ctx, "not-a-real-token-at-all", 1); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ConfirmTelegramLink (garbage code) = %v, want ErrInvalidToken", err)
	}
}

// fakeNotifier records every message it's asked to send and can be made to
// fail for specific chat ids.
type fakeNotifier struct {
	sent    []notification.Message
	failFor map[int64]bool
}

func (f *fakeNotifier) Send(_ context.Context, m notification.Message) error {
	if f.failFor[m.ChatID] {
		return errors.New("fake delivery failure")
	}
	f.sent = append(f.sent, m)
	return nil
}

func TestServiceNotifyNewJobs_MatchesOnAnyKeywordAndIsIdempotent(t *testing.T) {
	pool := newPool(t)
	svc := newTestService(t, pool, Config{})
	ctx := context.Background()

	golangFan, _, _ := mustLogin(t, svc.store, "golang-fan@example.com")
	if err := svc.SetNotificationKeywords(ctx, golangFan.ID, []string{"golang", "devops"}); err != nil {
		t.Fatal(err)
	}
	_, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.CreateTelegramLinkToken(ctx, golangFan.ID, hash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ConfirmTelegramLink(ctx, hash, 111); err != nil {
		t.Fatal(err)
	}

	noKeywords, _, _ := mustLogin(t, svc.store, "no-keywords@example.com")
	_, hash2, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.CreateTelegramLinkToken(ctx, noKeywords.ID, hash2, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ConfirmTelegramLink(ctx, hash2, 222); err != nil {
		t.Fatal(err)
	}

	candidates := []notification.Candidate{
		{JobID: seedJob(t, pool, "open"), Title: "Senior Backend Engineer (Golang)"},
		{JobID: seedJob(t, pool, "open"), Title: "Product Marketing Manager"},
	}
	notifier := &fakeNotifier{}
	if err := svc.NotifyNewJobs(ctx, candidates, notifier); err != nil {
		t.Fatalf("NotifyNewJobs: %v", err)
	}
	if len(notifier.sent) != 1 || notifier.sent[0].ChatID != 111 {
		t.Fatalf("sent = %+v, want exactly one message to chat 111 (the golang job)", notifier.sent)
	}

	// A second tick with the same candidates sends nothing more.
	if err := svc.NotifyNewJobs(ctx, candidates, notifier); err != nil {
		t.Fatalf("NotifyNewJobs (second tick): %v", err)
	}
	if len(notifier.sent) != 1 {
		t.Errorf("sent after second tick = %d messages, want still 1 (idempotent)", len(notifier.sent))
	}
}

func TestServiceNotifyNewJobs_AFailedSendIsNotRecordedAndRetriesNextTick(t *testing.T) {
	pool := newPool(t)
	svc := newTestService(t, pool, Config{})
	ctx := context.Background()

	u, _, _ := mustLogin(t, svc.store, "retry@example.com")
	if err := svc.SetNotificationKeywords(ctx, u.ID, []string{"golang"}); err != nil {
		t.Fatal(err)
	}
	_, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.CreateTelegramLinkToken(ctx, u.ID, hash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ConfirmTelegramLink(ctx, hash, 333); err != nil {
		t.Fatal(err)
	}

	candidates := []notification.Candidate{{JobID: seedJob(t, pool, "open"), Title: "Golang Engineer"}}
	failing := &fakeNotifier{failFor: map[int64]bool{333: true}}
	if err := svc.NotifyNewJobs(ctx, candidates, failing); err != nil {
		t.Fatalf("NotifyNewJobs (failing send): %v", err)
	}
	if len(failing.sent) != 0 {
		t.Fatalf("sent = %+v, want none (delivery failed)", failing.sent)
	}

	working := &fakeNotifier{}
	if err := svc.NotifyNewJobs(ctx, candidates, working); err != nil {
		t.Fatalf("NotifyNewJobs (retry): %v", err)
	}
	if len(working.sent) != 1 {
		t.Fatalf("sent on retry = %+v, want exactly one message (nothing was recorded after the earlier failure)", working.sent)
	}
}

func TestJavaDoesNotMatchJavaScriptInNotifyNewJobs(t *testing.T) {
	pool := newPool(t)
	svc := newTestService(t, pool, Config{})
	ctx := context.Background()
	u, _, _ := mustLogin(t, svc.store, "java-fan@"+strconv.Itoa(int(time.Now().UnixNano()))+".example.com")
	if err := svc.SetNotificationKeywords(ctx, u.ID, []string{"java"}); err != nil {
		t.Fatal(err)
	}
	_, hash, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.store.CreateTelegramLinkToken(ctx, u.ID, hash, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.ConfirmTelegramLink(ctx, hash, 444); err != nil {
		t.Fatal(err)
	}

	notifier := &fakeNotifier{}
	err = svc.NotifyNewJobs(ctx, []notification.Candidate{{JobID: seedJob(t, pool, "open"), Title: "JavaScript Developer"}}, notifier)
	if err != nil {
		t.Fatal(err)
	}
	if len(notifier.sent) != 0 {
		t.Errorf("sent = %+v, want none: \"java\" must not whole-word match \"JavaScript\"", notifier.sent)
	}
}
