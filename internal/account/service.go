package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/relevance"
	"github.com/Bantamlak12/remote-job-aggregator/internal/mailer"
	"github.com/Bantamlak12/remote-job-aggregator/internal/notification"
	"github.com/Bantamlak12/remote-job-aggregator/internal/ratelimit"
)

// Config holds the service's policy. Zero values are replaced by the defaults in
// Defaults.
type Config struct {
	// PublicBaseURL is the origin of the web app (https://jobs.example.com): the
	// sign-in link is built from it, never from a request header.
	PublicBaseURL string
	// LoginTokenTTL is how long an emailed link works.
	LoginTokenTTL time.Duration
	// SessionTTL is how long a session lasts without use; SessionMaxAge caps a
	// session's total life however often it is used.
	SessionTTL, SessionMaxAge time.Duration
	// MaxBookmarks is the most jobs one account may save.
	MaxBookmarks int
	// Login-link requests allowed per address (in 15 minutes and in a day), per
	// client IP (in 15 minutes and in a day) and in total (per hour), and attempts
	// to open a link per client IP per 15 minutes.
	LoginPerEmail15Min, LoginPerEmailDay, LoginPerIP15Min, LoginPerIPDay, LoginGlobalHour, VerifyPerIP15Min int
	// MailWorkers and MailQueue bound the outgoing mail: workers sending, and how
	// many messages may wait.
	MailWorkers, MailQueue int
	// MaxSessions is the most live sessions one account keeps: signing in on one more
	// device ends the oldest.
	MaxSessions int
	// SiteName is what the email calls the site.
	SiteName string
	// TelegramLinkTokenTTL is how long a requested link code works.
	TelegramLinkTokenTTL time.Duration
	// TelegramLinkPerHour caps how many link codes one account may request
	// per hour (the same "circuit breaker" reasoning as the sign-in link
	// limits: a code costs nothing to generate but is worth bounding anyway).
	TelegramLinkPerHour int
	// TelegramBotUsername builds the t.me deep link a requested code is
	// shown with (https://t.me/<username>?start=<code>). Notifications stay
	// entirely unavailable (RequestTelegramLink refuses) until this is set.
	TelegramBotUsername string
	// MaxNotificationKeywords is the most keywords one account may subscribe to.
	MaxNotificationKeywords int
}

// Defaults returns the policy a production site should start from.
func Defaults() Config {
	return Config{
		LoginTokenTTL: 10 * time.Minute,
		SessionTTL:    14 * 24 * time.Hour, SessionMaxAge: 30 * 24 * time.Hour,
		MaxBookmarks:       500,
		LoginPerEmail15Min: 3, LoginPerEmailDay: 10, LoginPerIP15Min: 10, LoginPerIPDay: 50, LoginGlobalHour: 300, VerifyPerIP15Min: 30,
		MailWorkers: 2, MailQueue: 64, MaxSessions: 10,
		SiteName:                "Remote jobs",
		TelegramLinkTokenTTL:    15 * time.Minute,
		TelegramLinkPerHour:     5,
		MaxNotificationKeywords: 20,
	}
}

func (c Config) withDefaults() Config {
	d := Defaults()
	if c.LoginTokenTTL <= 0 {
		c.LoginTokenTTL = d.LoginTokenTTL
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = d.SessionTTL
	}
	if c.SessionMaxAge <= 0 {
		c.SessionMaxAge = d.SessionMaxAge
	}
	if c.MaxBookmarks <= 0 {
		c.MaxBookmarks = d.MaxBookmarks
	}
	set := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	set(&c.LoginPerEmail15Min, d.LoginPerEmail15Min)
	set(&c.LoginPerEmailDay, d.LoginPerEmailDay)
	set(&c.LoginPerIP15Min, d.LoginPerIP15Min)
	set(&c.LoginPerIPDay, d.LoginPerIPDay)
	set(&c.LoginGlobalHour, d.LoginGlobalHour)
	set(&c.VerifyPerIP15Min, d.VerifyPerIP15Min)
	if c.MailWorkers <= 0 {
		c.MailWorkers = d.MailWorkers
	}
	if c.MailQueue <= 0 {
		c.MailQueue = d.MailQueue
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = d.MaxSessions
	}
	if c.SiteName == "" {
		c.SiteName = d.SiteName
	}
	if c.TelegramLinkTokenTTL <= 0 {
		c.TelegramLinkTokenTTL = d.TelegramLinkTokenTTL
	}
	set(&c.TelegramLinkPerHour, d.TelegramLinkPerHour)
	set(&c.MaxNotificationKeywords, d.MaxNotificationKeywords)
	c.PublicBaseURL = strings.TrimRight(c.PublicBaseURL, "/")
	return c
}

// RateLimitedError says how long until the caller may try again. Global is true when
// the site-wide cap on emailed links was what refused the request (the "circuit
// breaker": an operator should hear about it), false for a per-address or per-client limit.
type RateLimitedError struct {
	RetryAfter time.Duration
	Global     bool
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("account: rate limited, retry in %s", e.RetryAfter.Round(time.Second))
}

// ErrBusy is returned when the outgoing mail queue is full.
var ErrBusy = errors.New("account: mail queue is full")

// JobVisibility says whether the public can see a job. Saving a job requires it, so
// the saved-jobs feature cannot be used to read a job the public cannot.
type JobVisibility interface {
	Visible(ctx context.Context, jobID int64) (bool, error)
}

// Service is sign-in, sessions and bookmarks over a Store.
type Service struct {
	store  *Store
	mail   mailer.Mailer
	cfg    Config
	logger *slog.Logger
	now    func() time.Time
	jobs   JobVisibility

	emailShort, emailDay, ipShort, ipDay, global, verifyIP *ratelimit.Limiter
	queue                                                  chan mailer.Message

	telegramLink *ratelimit.Limiter
}

// NewService returns a Service. Call Run to start the mail workers.
func NewService(store *Store, m mailer.Mailer, cfg Config, logger *slog.Logger) *Service {
	cfg = cfg.withDefaults()
	const maxKeys = 50_000
	return &Service{
		store: store, mail: m, cfg: cfg, logger: logger, now: time.Now,
		emailShort: ratelimit.New(cfg.LoginPerEmail15Min, 15*time.Minute, maxKeys),
		emailDay:   ratelimit.New(cfg.LoginPerEmailDay, 24*time.Hour, maxKeys),
		ipShort:    ratelimit.New(cfg.LoginPerIP15Min, 15*time.Minute, maxKeys),
		ipDay:      ratelimit.New(cfg.LoginPerIPDay, 24*time.Hour, maxKeys),
		global:     ratelimit.New(cfg.LoginGlobalHour, time.Hour, 1),
		verifyIP:   ratelimit.New(cfg.VerifyPerIP15Min, 15*time.Minute, maxKeys),
		queue:      make(chan mailer.Message, cfg.MailQueue),

		telegramLink: ratelimit.New(cfg.TelegramLinkPerHour, time.Hour, maxKeys),
	}
}

// WithJobs makes Save check that the job is publicly visible, as the job store
// defines it (open, not past its deadline, within the age limit). Without it only the
// store's own "job is open" check applies.
func (s *Service) WithJobs(v JobVisibility) *Service {
	s.jobs = v
	return s
}

// sendTimeout bounds one attempt to deliver one email.
const sendTimeout = 10 * time.Second

// Config returns the policy in force.
func (s *Service) Config() Config { return s.cfg }

// Run sends queued mail and purges expired rows until ctx is done, then returns
// after the workers stop. Messages still queued at shutdown are dropped (the
// person asks again).
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.MailWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case m := <-s.queue:
					sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
					if err := s.mail.Send(sendCtx, m); err != nil {
						// The address is not logged: it is personal data, and the domain is enough to see a provider problem.
						s.logger.Error("sending a sign-in email failed", "to_domain", domainOf(m.To), "error", err)
					}
					cancel()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		purge := func() {
			if n, err := s.store.Purge(ctx, s.now().Add(-24*time.Hour)); err != nil {
				s.logger.Error("purging expired sign-in rows failed", "error", err)
			} else if n > 0 {
				s.logger.Info("purged expired sign-in rows", "rows", n)
			}
		}
		purge() // at startup too: a server restarted more often than hourly must not skip it
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				purge()
			}
		}
	}()
	wg.Wait()
}

func domainOf(addr string) string {
	if at := strings.LastIndexByte(addr, '@'); at >= 0 {
		return addr[at+1:]
	}
	return ""
}

// RequestLogin emails a sign-in link to the address. It answers the same
// whether or not the address has an account (nothing is looked up), is limited
// per address and per client IP, and never returns the link.
func (s *Service) RequestLogin(ctx context.Context, rawEmail, clientIP string) error {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return err
	}
	// Aliases share one budget: a+1@gmail.com and a.b@gmail.com are one mailbox.
	key := throttleKey(email)
	for _, l := range []struct {
		lim    *ratelimit.Limiter
		key    string
		global bool
	}{{s.ipShort, clientIP, false}, {s.ipDay, clientIP, false}, {s.emailShort, key, false}, {s.emailDay, key, false}, {s.global, "all", true}} {
		if ok, wait := l.lim.Allow(l.key); !ok {
			return &RateLimitedError{RetryAfter: wait, Global: l.global}
		}
	}

	token, hash, err := newToken()
	if err != nil {
		return err
	}
	// The link is worth mailing only once it can be spent, so it is stored first. The
	// per-address and site-wide limits are checked again here, in the database and
	// atomically with the insert: they survive a restart and hold across servers, and
	// concurrent requests cannot both slip under a limit.
	if err := s.store.CreateLoginTokenLimited(ctx, email, hash, s.cfg.LoginTokenTTL, LinkLimits{
		PerAddress15Min: s.cfg.LoginPerEmail15Min, PerAddressDay: s.cfg.LoginPerEmailDay, GlobalHour: s.cfg.LoginGlobalHour,
	}); err != nil {
		return err
	}
	select {
	case s.queue <- s.loginMessage(email, token):
		return nil
	default:
		return ErrBusy
	}
}

// loginMessage builds the email. The link puts the token in the URL fragment: a
// fragment is never sent to a server, so it is not in access logs, proxy logs or
// Referer headers; the page reads it and posts it to the API.
func (s *Service) loginMessage(email, token string) mailer.Message {
	link := s.cfg.PublicBaseURL + "/auth/verify#token=" + token
	mins := int(s.cfg.LoginTokenTTL.Minutes())
	text := fmt.Sprintf("Open this link to sign in to %s:\n\n%s\n\nIt works once and expires in %d minutes.\n\nIf you did not ask for this, ignore this email: nobody can sign in without the link.\n",
		s.cfg.SiteName, link, mins)
	html := fmt.Sprintf(`<p>Open this link to sign in to %s:</p><p><a href="%s">Sign in</a></p><p>It works once and expires in %d minutes.</p><p>If you did not ask for this, ignore this email: nobody can sign in without the link.</p>`,
		htmlEscape(s.cfg.SiteName), htmlEscape(link), mins)
	return mailer.Message{To: email, Subject: "Your sign-in link for " + s.cfg.SiteName, Text: text, HTML: html}
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

// Inspect says which address a sign-in link is for, masked ("a***@example.com"),
// without spending it: the page that asks the person to confirm shows it, so a
// link opened by a mail scanner or by someone else's browser does nothing until a
// person confirms. Rate limited like Verify; an unknown, used or expired link is
// ErrInvalidToken.
func (s *Service) Inspect(ctx context.Context, token, clientIP string) (string, error) {
	if ok, wait := s.verifyIP.Allow(clientIP); !ok {
		return "", &RateLimitedError{RetryAfter: wait}
	}
	// Whatever was sent goes down the same path (hash, database lookup), so a string
	// that cannot be a token takes as long to refuse as one that merely is not known.
	email, err := s.store.PeekLoginToken(ctx, hashToken(clampToken(token)))
	if err != nil {
		return "", err
	}
	return MaskEmail(email), nil
}

// MaskEmail shows the first character and the top-level domain only:
// "alice@mail.example.com" is "a***@***.com". Enough for a person to recognise their
// own address, not enough to tell a stranger who holds a link whose it is.
func MaskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	dot := strings.LastIndexByte(email, '.')
	if at <= 0 || dot < at {
		return "***"
	}
	return email[:1] + "***@***" + email[dot:]
}

// Verify spends a sign-in link and returns a new session token (to set as a
// cookie) and the user. Every failure is ErrInvalidToken, whatever the cause.
// previousSession is the session token the caller already holds, if any: signing in
// ends it, so a session cannot be carried across a sign-in (session fixation) and
// the old cookie stops working.
func (s *Service) Verify(ctx context.Context, token, clientIP, previousSession string) (sessionToken string, u User, err error) {
	if ok, wait := s.verifyIP.Allow(clientIP); !ok {
		return "", User{}, &RateLimitedError{RetryAfter: wait}
	}
	sess, sessHash, err := newToken()
	if err != nil {
		return "", User{}, err
	}
	var prev []byte
	if checkToken(previousSession) == nil {
		prev = hashToken(previousSession)
	}
	u, err = s.store.Login(ctx, hashToken(clampToken(token)), sessHash, s.cfg.SessionTTL, prev, s.cfg.MaxSessions)
	if err != nil {
		return "", User{}, err
	}
	return sess, u, nil
}

// Authenticate returns the user of a session token.
func (s *Service) Authenticate(ctx context.Context, sessionToken string) (User, error) {
	if checkToken(sessionToken) != nil {
		return User{}, ErrNoSession
	}
	return s.store.Authenticate(ctx, hashToken(sessionToken), s.cfg.SessionTTL, s.cfg.SessionMaxAge)
}

// Logout ends the session with this token.
func (s *Service) Logout(ctx context.Context, sessionToken string) error {
	if checkToken(sessionToken) != nil {
		return nil
	}
	return s.store.DeleteSession(ctx, hashToken(sessionToken))
}

// LogoutEverywhere ends every session of the user.
func (s *Service) LogoutEverywhere(ctx context.Context, userID int64) error {
	_, err := s.store.DeleteSessionsOfUser(ctx, userID)
	return err
}

// DeleteAccount erases the account, its sessions, bookmarks and pending links.
func (s *Service) DeleteAccount(ctx context.Context, userID int64) error {
	return s.store.DeleteUser(ctx, userID)
}

// Save saves a job for the user. Only a job the public can see may be saved.
func (s *Service) Save(ctx context.Context, userID, jobID int64) error {
	if s.jobs != nil {
		visible, err := s.jobs.Visible(ctx, jobID)
		if err != nil {
			return err
		}
		if !visible {
			// A job already saved when it went out of view stays saved (and saving it
			// again is a no-op), but a job never saved cannot be added.
			if saved, err := s.store.HasBookmark(ctx, userID, jobID); err != nil {
				return err
			} else if saved {
				return nil
			}
			return ErrJobNotFound
		}
	}
	return s.store.AddBookmark(ctx, userID, jobID, s.cfg.MaxBookmarks)
}

// Unsave removes a saved job.
func (s *Service) Unsave(ctx context.Context, userID, jobID int64) error {
	return s.store.RemoveBookmark(ctx, userID, jobID)
}

// Saved returns a page of the user's saved jobs and the total.
func (s *Service) Saved(ctx context.Context, userID int64, limit, offset int) ([]Bookmark, int, error) {
	return s.store.Bookmarks(ctx, userID, limit, offset)
}

// SavedIDs returns the ids of the user's saved jobs.
func (s *Service) SavedIDs(ctx context.Context, userID int64) ([]int64, error) {
	return s.store.BookmarkIDs(ctx, userID, s.cfg.MaxBookmarks)
}

// HasSaved reports whether the user saved the job.
func (s *Service) HasSaved(ctx context.Context, userID, jobID int64) (bool, error) {
	return s.store.HasBookmark(ctx, userID, jobID)
}

// ErrNotificationsNotConfigured is returned by RequestTelegramLink when the
// server has no bot username configured (TELEGRAM_BOT_TOKEN unset):
// notifications are then fully inert, the same "refuse outright" treatment
// search-discover gives a missing SERPER_API_KEY rather than pretending to
// work.
var ErrNotificationsNotConfigured = errors.New("account: telegram notifications are not configured")

// ErrUnknownKeyword is returned for a keyword outside relevance.RelevantVocabulary.
var ErrUnknownKeyword = errors.New("account: not a recognized notification keyword")

// ErrTooManyKeywords is returned when a keyword set would exceed the
// per-account limit.
var ErrTooManyKeywords = errors.New("account: too many notification keywords")

// NotificationKeywords returns an account's keyword subscriptions.
func (s *Service) NotificationKeywords(ctx context.Context, userID int64) ([]string, error) {
	return s.store.NotificationKeywords(ctx, userID)
}

// SetNotificationKeywords replaces an account's keyword subscriptions.
// Every keyword must be one relevance.RelevantVocabulary lists -- the same
// vocabulary the picker's options come from, so nothing reaches the
// database that could never match a title.
func (s *Service) SetNotificationKeywords(ctx context.Context, userID int64, keywords []string) error {
	if len(keywords) > s.cfg.MaxNotificationKeywords {
		return ErrTooManyKeywords
	}
	valid := make(map[string]bool)
	for _, v := range relevance.RelevantVocabulary() {
		valid[v] = true
	}
	seen := make(map[string]bool, len(keywords))
	deduped := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		if !valid[kw] {
			return ErrUnknownKeyword
		}
		if seen[kw] {
			continue
		}
		seen[kw] = true
		deduped = append(deduped, kw)
	}
	return s.store.SetNotificationKeywords(ctx, userID, deduped)
}

// TelegramLinkRequest is a freshly issued Telegram link: the deep link to
// open and how long it stays valid.
type TelegramLinkRequest struct {
	DeepLink  string
	ExpiresIn time.Duration
}

// RequestTelegramLink issues a single-use code for userID and returns the
// t.me deep link that spends it once opened. Rate limited per account,
// like requesting a sign-in link is limited per address.
func (s *Service) RequestTelegramLink(ctx context.Context, userID int64) (TelegramLinkRequest, error) {
	if s.cfg.TelegramBotUsername == "" {
		return TelegramLinkRequest{}, ErrNotificationsNotConfigured
	}
	if ok, wait := s.telegramLink.Allow(strconv.FormatInt(userID, 10)); !ok {
		return TelegramLinkRequest{}, &RateLimitedError{RetryAfter: wait}
	}
	token, hash, err := newToken()
	if err != nil {
		return TelegramLinkRequest{}, err
	}
	if err := s.store.CreateTelegramLinkToken(ctx, userID, hash, s.cfg.TelegramLinkTokenTTL); err != nil {
		return TelegramLinkRequest{}, err
	}
	return TelegramLinkRequest{
		DeepLink:  fmt.Sprintf("https://t.me/%s?start=%s", s.cfg.TelegramBotUsername, token),
		ExpiresIn: s.cfg.TelegramLinkTokenTTL,
	}, nil
}

// ConfirmTelegramLink implements notification.LinkConfirmer: it's called for
// every "/start <code>" the Telegram listener sees. Every failure but a
// chat already linked to a different account is logged and dropped, not
// surfaced anywhere -- there is no requester waiting on this call the way
// an HTTP handler's caller is; the person just sees the bot silently do
// nothing and can request a fresh code.
func (s *Service) ConfirmTelegramLink(ctx context.Context, code string, chatID int64) error {
	if checkToken(code) != nil {
		return ErrInvalidToken
	}
	_, err := s.store.ConfirmTelegramLink(ctx, hashToken(clampToken(code)), chatID)
	return err
}

// TelegramLinked reports whether an account has a confirmed Telegram chat.
func (s *Service) TelegramLinked(ctx context.Context, userID int64) (bool, error) {
	_, linked, err := s.store.TelegramChatID(ctx, userID)
	return linked, err
}

// UnlinkTelegram removes an account's Telegram link.
func (s *Service) UnlinkTelegram(ctx context.Context, userID int64) error {
	return s.store.UnlinkTelegram(ctx, userID)
}

// NotifyNewJobs is the scheduler's per-tick delivery step: for every account
// with a confirmed Telegram chat and at least one keyword, it whole-word
// matches each candidate's title against that account's keywords (any one
// matching notifies -- OR, not AND) and sends the ones not already recorded
// in notified_jobs. A send that fails leaves no record, so the next tick
// retries it for free. O(subscribers x candidates): deliberately simple,
// this project's scale needs nothing smarter (CLAUDE.md: vanilla by default).
func (s *Service) NotifyNewJobs(ctx context.Context, candidates []notification.Candidate, notifier notification.Notifier) error {
	if len(candidates) == 0 {
		return nil
	}
	subs, err := s.store.subscribers(ctx)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return nil
	}

	tokenized := make([][]string, len(candidates))
	for i, c := range candidates {
		tokenized[i] = relevance.Words(c.Title)
	}

	var errs []error
	for _, sub := range subs {
		jobIDs := make([]int64, len(candidates))
		for i, c := range candidates {
			jobIDs[i] = c.JobID
		}
		already, err := s.store.notifiedJobIDs(ctx, sub.userID, jobIDs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for i, c := range candidates {
			if already[c.JobID] {
				continue
			}
			if !matchesAny(tokenized[i], sub.keywords) {
				continue
			}
			msg := notification.Message{ChatID: sub.chatID, Text: notifyText(c.Title)}
			if err := notifier.Send(ctx, msg); err != nil {
				s.logger.Error("sending a job notification failed", "error", err)
				continue
			}
			if err := s.store.recordNotified(ctx, sub.userID, c.JobID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// matchesAny reports whether any keyword whole-word matches a title's words.
func matchesAny(titleWords []string, keywords []string) bool {
	for _, kw := range keywords {
		if relevance.ContainsPhrase(titleWords, kw) {
			return true
		}
	}
	return false
}

func notifyText(title string) string {
	return fmt.Sprintf("New job matching your interests: %s", title)
}
