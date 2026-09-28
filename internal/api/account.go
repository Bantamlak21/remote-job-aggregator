package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/account"
	"github.com/Bantamlak12/remote-job-aggregator/internal/filtering/relevance"
	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
)

// Accounts is what the account endpoints need of internal/account.Service.
type Accounts interface {
	RequestLogin(ctx context.Context, email, clientIP string) error
	Inspect(ctx context.Context, token, clientIP string) (string, error)
	Verify(ctx context.Context, token, clientIP, previousSession string) (string, account.User, error)
	Authenticate(ctx context.Context, sessionToken string) (account.User, error)
	Logout(ctx context.Context, sessionToken string) error
	LogoutEverywhere(ctx context.Context, userID int64) error
	DeleteAccount(ctx context.Context, userID int64) error
	Save(ctx context.Context, userID, jobID int64) error
	Unsave(ctx context.Context, userID, jobID int64) error
	Saved(ctx context.Context, userID int64, limit, offset int) ([]account.Bookmark, int, error)
	SavedIDs(ctx context.Context, userID int64) ([]int64, error)
	HasSaved(ctx context.Context, userID, jobID int64) (bool, error)
	NotificationKeywords(ctx context.Context, userID int64) ([]string, error)
	SetNotificationKeywords(ctx context.Context, userID int64, keywords []string) error
	RequestTelegramLink(ctx context.Context, userID int64) (account.TelegramLinkRequest, error)
	TelegramLinked(ctx context.Context, userID int64) (bool, error)
	UnlinkTelegram(ctx context.Context, userID int64) error
}

// SavedJobs reads jobs for an account's saved list, in any state.
type SavedJobs interface {
	ListByIDs(ctx context.Context, ids []int64) ([]job.Job, error)
	GetForOwner(ctx context.Context, id string) (job.Job, error)
}

// CookieConfig is how the session cookie is set.
type CookieConfig struct {
	// Secure marks the cookie HTTPS-only and gives it the "__Host-" prefix, which
	// makes a browser refuse it unless it was set over HTTPS by this exact host with
	// Path=/ and no Domain. Turn it off only for local development over http.
	Secure bool
	// SameSite is Lax (the default) or Strict. The web app and the API are served from
	// one site, so the cookie is first-party.
	SameSite http.SameSite
}

func (c CookieConfig) name() string {
	if c.Secure {
		return "__Host-session"
	}
	return "session"
}

func (c CookieConfig) set(w http.ResponseWriter, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: c.name(), Value: token, Path: "/", HttpOnly: true, Secure: c.Secure, SameSite: c.sameSite(),
		MaxAge: int(ttl.Seconds()),
	})
}

func (c CookieConfig) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: c.name(), Value: "", Path: "/", HttpOnly: true, Secure: c.Secure, SameSite: c.sameSite(), MaxAge: -1})
}

func (c CookieConfig) sameSite() http.SameSite {
	if c.SameSite == 0 {
		return http.SameSiteLaxMode
	}
	return c.SameSite
}

// accountRoutes carries what the account handlers share.
type accountRoutes struct {
	accounts   Accounts
	saved      SavedJobs
	cookie     CookieConfig
	sessionTTL time.Duration
	// clientIPHeader names a header a trusted reverse proxy sets to the real client
	// address ("" means use the connection's address: never trust a header the
	// client can set).
	clientIPHeader string
	logger         *slog.Logger
	// lastBreaker is when (unix seconds) the site-wide mail cap last raised the
	// mail_breaker_open alert: it is raised at most once a minute, not once per refused request.
	lastBreaker atomic.Int64
}

type ctxKey int

const userKey ctxKey = 1

func userFrom(ctx context.Context) (account.User, bool) {
	u, ok := ctx.Value(userKey).(account.User)
	return u, ok
}

// mount registers the endpoints.
func (a *accountRoutes) mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/auth/login", a.noStore(a.login))
	mux.HandleFunc("POST /api/v1/auth/inspect", a.noStore(a.inspect))
	mux.HandleFunc("POST /api/v1/auth/verify", a.noStore(a.verify))
	mux.HandleFunc("POST /api/v1/auth/logout", a.noStore(a.logout))
	mux.HandleFunc("POST /api/v1/auth/logout-all", a.noStore(a.signedIn(a.logoutAll)))
	mux.HandleFunc("GET /api/v1/me", a.noStore(a.signedIn(a.me)))
	mux.HandleFunc("DELETE /api/v1/me", a.noStore(a.signedIn(a.deleteMe)))
	mux.HandleFunc("GET /api/v1/me/bookmarks", a.noStore(a.signedIn(a.listBookmarks)))
	mux.HandleFunc("GET /api/v1/me/bookmarks/ids", a.noStore(a.signedIn(a.bookmarkIDs)))
	mux.HandleFunc("PUT /api/v1/me/bookmarks/{id}", a.noStore(a.signedIn(a.putBookmark)))
	mux.HandleFunc("DELETE /api/v1/me/bookmarks/{id}", a.noStore(a.signedIn(a.deleteBookmark)))
	mux.HandleFunc("GET /api/v1/notification-keywords", a.noStore(notificationVocabularyHandler))
	mux.HandleFunc("GET /api/v1/me/notifications", a.noStore(a.signedIn(a.getNotifications)))
	mux.HandleFunc("PUT /api/v1/me/notifications", a.noStore(a.signedIn(a.putNotifications)))
	mux.HandleFunc("POST /api/v1/me/telegram/link", a.noStore(a.signedIn(a.requestTelegramLink)))
	mux.HandleFunc("GET /api/v1/me/telegram/status", a.noStore(a.signedIn(a.telegramStatus)))
	mux.HandleFunc("DELETE /api/v1/me/telegram/link", a.noStore(a.signedIn(a.deleteTelegramLink)))
}

// log is the request's logger (it carries the request id).
func (a *accountRoutes) log(r *http.Request) *slog.Logger { return reqLogger(r, a.logger) }

// event records a security-relevant event at info level with the request id and no
// secret: never a token, a cookie or a full address (pass the masked one).
func (a *accountRoutes) event(r *http.Request, name string, attrs ...any) {
	a.log(r).Info("auth event", append([]any{"event", name}, attrs...)...)
}

// noStore keeps every response of these endpoints out of every cache: they are
// per person, and some carry a session cookie. (withSecurityHeaders does the same for
// every response under the prefix, including the ones no handler answers.)
func (a *accountRoutes) noStore(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next(w, r)
	}
}

// signedIn requires a live session and passes the user on in the context. It also
//   - slides the cookie: when the session was just extended, the cookie is sent again
//     with a fresh Max-Age, or the browser would drop it at the original time however
//     often the person comes back;
//   - answers 401 (and clears a cookie that no longer works) for a missing or ended
//     session, but 503 for a database that cannot say: an outage must not look like
//     "you were signed out";
//   - reads the whole body (at most 1 KiB) before the handler runs, so an oversized
//     body is a 413 even on routes that read none.
func (a *accountRoutes) signedIn(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, cookieValue, err := a.userOf(r)
		switch {
		case err == nil:
		case errors.Is(err, errNoCookie):
			writeCode(w, http.StatusUnauthorized, "unauthorized", "sign in required")
			return
		case errors.Is(err, account.ErrNoSession):
			a.cookie.clear(w)
			writeCode(w, http.StatusUnauthorized, "unauthorized", "sign in required")
			return
		default:
			if clientGone(r, err) {
				return
			}
			a.log(r).Error("authenticating a session failed", "error", err)
			writeCode(w, http.StatusServiceUnavailable, "internal_error", "temporarily unavailable, try again")
			return
		}
		if !readSmallBody(w, r) {
			return
		}
		if u.SessionRenewed {
			if left := time.Until(u.SessionExpires); left > 0 {
				a.cookie.set(w, cookieValue, left)
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// maxSignedInBody is the most a signed-in route accepts: none of them takes more than a
// tiny JSON object.
const maxSignedInBody = 1 << 10

// readSmallBody reads the whole request body (at most maxSignedInBody) into memory and
// puts it back for the handler, so a route that reads no body still refuses an
// oversized one with a 413 instead of quietly ignoring it. It answers when it refuses
// and reports whether the handler may go on.
func readSmallBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength > maxSignedInBody {
		writeCode(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSignedInBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeCode(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
		} else {
			writeCode(w, http.StatusBadRequest, "bad_request", "invalid request body")
		}
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return true
}

// errNoCookie is returned by userOf when the request has no session cookie.
var errNoCookie = errors.New("api: no session cookie")

// userOf resolves the session cookie to a user, and returns the cookie's value (to be
// re-sent when the session slides). The error is errNoCookie (none sent),
// account.ErrNoSession (sent but not valid) or another one (the lookup itself failed).
func (a *accountRoutes) userOf(r *http.Request) (account.User, string, error) {
	c, err := r.Cookie(a.cookie.name())
	if err != nil || c.Value == "" {
		return account.User{}, "", errNoCookie
	}
	u, err := a.accounts.Authenticate(r.Context(), c.Value)
	if err != nil {
		return account.User{}, "", err
	}
	return u, c.Value, nil
}

// clientIP is the address rate limits are counted against.
//
// With no clientIPHeader it is the connection's address. With one (the operator says a
// trusted reverse proxy sets it) it is what that proxy wrote: for X-Forwarded-For the
// LAST entry, the one the single trusted proxy appended (the entries before it are
// whatever the client sent and are never trusted); every other allowed header is one
// value the proxy overwrites. An IPv6 address is reduced to its /64, the smallest block
// one subscriber holds, so a single household cannot mint 2^64 "different" clients.
func (a *accountRoutes) clientIP(r *http.Request) string {
	if a.clientIPHeader != "" {
		v := r.Header.Get(a.clientIPHeader)
		if strings.EqualFold(a.clientIPHeader, "X-Forwarded-For") {
			parts := strings.Split(v, ",")
			v = parts[len(parts)-1]
		}
		if ip := net.ParseIP(strings.TrimSpace(v)); ip != nil {
			return foldIP(ip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return foldIP(ip)
	}
	return host
}

// foldIP is the rate-limit key of an address: IPv4 as is, IPv6 as its /64 prefix.
func foldIP(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

// maxAuthBody is the most a login, inspect or verify body may be: an address or a token.
const maxAuthBody = 1 << 10

// decodeJSON reads a small JSON body into v: media type exactly application/json,
// unknown fields refused, exactly one JSON value (anything after it, even a stray
// bracket, is refused), size capped.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if !isJSONMediaType(r.Header.Get("Content-Type")) {
		writeCode(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		// One value only: the next read must be the end of the body.
		var extra json.RawMessage
		if e := dec.Decode(&extra); !errors.Is(e, io.EOF) {
			err = errors.New("trailing data")
			if e != nil {
				var tooBig *http.MaxBytesError
				if errors.As(e, &tooBig) {
					err = e
				}
			}
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeCode(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
			return false
		}
		writeCode(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return false
	}
	return true
}

type userDTO struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

func toUserDTO(u account.User) userDTO {
	return userDTO{ID: strconv.FormatInt(u.ID, 10), Email: u.Email}
}

func (a *accountRoutes) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	err := a.accounts.RequestLogin(r.Context(), body.Email, a.clientIP(r))
	var limited *account.RateLimitedError
	switch {
	case err == nil:
		// The same answer whether or not the address has an account.
		a.event(r, "login_requested", "email", maskedOrEmpty(body.Email))
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
	case errors.Is(err, account.ErrInvalidEmail):
		writeCode(w, http.StatusBadRequest, "invalid_email", "enter a valid email address")
	case errors.As(err, &limited):
		a.event(r, "login_rate_limited")
		if limited.Global {
			a.breakerOpen(r)
		}
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())+1))
		writeCode(w, http.StatusTooManyRequests, "rate_limited", "too many sign-in requests, try again later")
	case errors.Is(err, account.ErrBusy):
		a.event(r, "mail_queue_full")
		w.Header().Set("Retry-After", "30")
		writeCode(w, http.StatusServiceUnavailable, "mail_busy", "cannot send email right now, try again in a minute")
	default:
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("requesting a sign-in link failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

// breakerOpen raises the alert for the site-wide cap on emailed links: something is
// sending far more sign-in mail than a real audience would (or the audience grew past
// the cap). Warn level, at most once a minute, so an alert rule can key on it without
// the flood that caused it filling the log.
func (a *accountRoutes) breakerOpen(r *http.Request) {
	now := time.Now().Unix()
	last := a.lastBreaker.Load()
	if now-last >= 60 && a.lastBreaker.CompareAndSwap(last, now) {
		a.log(r).Warn("auth event", "event", "mail_breaker_open")
	}
}

// inspect says which address a link is for (masked) without spending it, so the page
// can ask the person to confirm: a mail scanner that opens the link, or a link
// opened in someone else's browser, signs nobody in.
func (a *accountRoutes) inspect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	masked, err := a.accounts.Inspect(r.Context(), body.Token, a.clientIP(r))
	var limited *account.RateLimitedError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"email_masked": masked})
	case errors.Is(err, account.ErrInvalidToken):
		writeCode(w, http.StatusBadRequest, "invalid_link", "this sign-in link is invalid or has expired: request a new one")
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())+1))
		writeCode(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
	default:
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("inspecting a sign-in link failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func (a *accountRoutes) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// A session cookie sent with the sign-in is ended by it: signing in replaces the
	// session, so the old cookie stops working.
	previous := ""
	if c, err := r.Cookie(a.cookie.name()); err == nil {
		previous = c.Value
	}
	session, u, err := a.accounts.Verify(r.Context(), body.Token, a.clientIP(r), previous)
	var limited *account.RateLimitedError
	switch {
	case err == nil:
		a.event(r, "login_verified", "user_id", u.ID)
		a.cookie.set(w, session, a.sessionTTL)
		writeJSON(w, http.StatusOK, map[string]any{"user": toUserDTO(u)})
	case errors.Is(err, account.ErrInvalidToken):
		a.event(r, "login_failed")
		writeCode(w, http.StatusBadRequest, "invalid_link", "this sign-in link is invalid or has expired: request a new one")
	case errors.As(err, &limited):
		a.event(r, "login_rate_limited")
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())+1))
		writeCode(w, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
	default:
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("verifying a sign-in link failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func (a *accountRoutes) logout(w http.ResponseWriter, r *http.Request) {
	if !readSmallBody(w, r) {
		return
	}
	if c, err := r.Cookie(a.cookie.name()); err == nil {
		if err := a.accounts.Logout(r.Context(), c.Value); err != nil {
			a.log(r).Error("signing out failed", "error", err)
		}
	}
	a.event(r, "logout")
	a.cookie.clear(w)
	w.WriteHeader(http.StatusNoContent)
}

func (a *accountRoutes) logoutAll(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	if err := a.accounts.LogoutEverywhere(r.Context(), u.ID); err != nil {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("signing out everywhere failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	a.event(r, "logout_all", "user_id", u.ID)
	a.cookie.clear(w)
	w.WriteHeader(http.StatusNoContent)
}

func (a *accountRoutes) me(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"user": toUserDTO(u)})
}

// deleteMe erases the account. It needs the body {"confirm": true}: a request with
// none (or anything else) is refused, so it cannot be done by a stray call.
func (a *accountRoutes) deleteMe(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	const need = `send {"confirm": true} to delete the account`
	// Only an exact {"confirm": true} may delete. Every other body — missing, empty,
	// wrong content type, `{}`, `{"confirm": false}`, malformed JSON, trailing data — is
	// the same answer, confirmation_required: the one exception is a body so large it is
	// its own, unrelated abuse concern (413), never mistaken for a missing confirmation.
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthBody)
	var body struct {
		Confirm bool `json:"confirm"`
	}
	var tooBig *http.MaxBytesError
	if !isJSONMediaType(r.Header.Get("Content-Type")) {
		writeCode(w, http.StatusBadRequest, "confirmation_required", need)
		return
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(&body)
	if err == nil {
		var extra json.RawMessage
		if e := dec.Decode(&extra); !errors.Is(e, io.EOF) {
			err = e
			if err == nil {
				err = errors.New("trailing data")
			}
		}
	}
	if errors.As(err, &tooBig) {
		writeCode(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body too large")
		return
	}
	if err != nil || !body.Confirm {
		writeCode(w, http.StatusBadRequest, "confirmation_required", need)
		return
	}
	if err := a.accounts.DeleteAccount(r.Context(), u.ID); err != nil && !errors.Is(err, account.ErrNoUser) {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("deleting an account failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	a.event(r, "account_deleted", "user_id", u.ID)
	a.cookie.clear(w)
	w.WriteHeader(http.StatusNoContent)
}

// savedJobDTO is a saved job: the usual summary plus whether it is no longer publicly
// listed (closed on its board, past its deadline or older than the age limit) and when
// it was saved.
type savedJobDTO struct {
	jobSummaryDTO
	Closed  bool   `json:"closed"`
	SavedAt string `json:"saved_at"`
}

type savedListResponse struct {
	Jobs     []savedJobDTO `json:"jobs"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Total    int           `json:"total"`
}

func (a *accountRoutes) listBookmarks(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	page, size, err := parsePaging(r.URL.Query())
	if err != nil {
		writeCode(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	marks, total, err := a.accounts.Saved(r.Context(), u.ID, size, (page-1)*size)
	if err != nil {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("listing saved jobs failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	ids := make([]int64, len(marks))
	for i, m := range marks {
		ids[i] = m.JobID
	}
	jobs, err := a.saved.ListByIDs(r.Context(), ids)
	if err != nil {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("reading saved jobs failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	byID := make(map[string]job.Job, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
	}
	out := make([]savedJobDTO, 0, len(marks))
	for _, m := range marks {
		// Every bookmark has a job row (the foreign key cascades a deleted job away), so
		// the only way one is missing is a job deleted between the two reads: skip it.
		if j, ok := byID[strconv.FormatInt(m.JobID, 10)]; ok {
			out = append(out, savedJobDTO{jobSummaryDTO: toSummaryDTO(j), Closed: j.Closed, SavedAt: m.SavedAt.Format(time.RFC3339)})
		}
	}
	writeJSON(w, http.StatusOK, savedListResponse{Jobs: out, Page: page, PageSize: size, Total: total})
}

func (a *accountRoutes) bookmarkIDs(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	ids, err := a.accounts.SavedIDs(r.Context(), u.ID)
	if err != nil {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("listing saved ids failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ids": out})
}

func jobIDParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (a *accountRoutes) putBookmark(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := jobIDParam(r)
	if !ok {
		writeCode(w, http.StatusNotFound, "job_unavailable", "job not found")
		return
	}
	switch err := a.accounts.Save(r.Context(), u.ID, id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, account.ErrJobNotFound):
		writeCode(w, http.StatusNotFound, "job_unavailable", "job not found or no longer listed")
	case errors.Is(err, account.ErrBookmarkLimit):
		writeCode(w, http.StatusConflict, "bookmark_limit", "you have reached the limit of saved jobs: remove some to save more")
	case errors.Is(err, account.ErrNoUser):
		a.cookie.clear(w)
		writeCode(w, http.StatusUnauthorized, "unauthorized", "sign in required")
	default:
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("saving a job failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func (a *accountRoutes) deleteBookmark(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	id, ok := jobIDParam(r)
	if !ok {
		w.WriteHeader(http.StatusNoContent) // nothing to remove
		return
	}
	if err := a.accounts.Unsave(r.Context(), u.ID, id); err != nil {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("removing a saved job failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notificationVocabularyHandler returns the picker's option list: the same
// phrase vocabulary internal/filtering/relevance's role classifier uses.
// Public (no signedIn wrapper) but only mounted when accounts are on --
// there is nothing to subscribe to without them. The frontend never
// hardcodes this list.
func notificationVocabularyHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keywords": relevance.RelevantVocabulary()})
}

func (a *accountRoutes) getNotifications(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	keywords, err := a.accounts.NotificationKeywords(r.Context(), u.ID)
	if err != nil {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("reading notification keywords failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keywords": keywords})
}

func (a *accountRoutes) putNotifications(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	var body struct {
		Keywords []string `json:"keywords"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	switch err := a.accounts.SetNotificationKeywords(r.Context(), u.ID, body.Keywords); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, account.ErrUnknownKeyword):
		writeCode(w, http.StatusBadRequest, "unknown_keyword", "one of those keywords is not in the notification-keywords list")
	case errors.Is(err, account.ErrTooManyKeywords):
		writeCode(w, http.StatusBadRequest, "too_many_keywords", "too many notification keywords")
	default:
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("setting notification keywords failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func (a *accountRoutes) requestTelegramLink(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	req, err := a.accounts.RequestTelegramLink(r.Context(), u.ID)
	var limited *account.RateLimitedError
	switch {
	case err == nil:
		a.event(r, "telegram_link_requested", "user_id", u.ID)
		writeJSON(w, http.StatusOK, map[string]any{
			"deep_link":          req.DeepLink,
			"expires_in_seconds": int(req.ExpiresIn.Seconds()),
		})
	case errors.Is(err, account.ErrNotificationsNotConfigured):
		writeCode(w, http.StatusServiceUnavailable, "notifications_not_configured", "telegram notifications are not set up on this server")
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())+1))
		writeCode(w, http.StatusTooManyRequests, "rate_limited", "too many link requests, try again later")
	default:
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("requesting a telegram link failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
	}
}

func (a *accountRoutes) telegramStatus(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	linked, err := a.accounts.TelegramLinked(r.Context(), u.ID)
	if err != nil {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("reading telegram link status failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"linked": linked})
}

func (a *accountRoutes) deleteTelegramLink(w http.ResponseWriter, r *http.Request) {
	u, _ := userFrom(r.Context())
	if err := a.accounts.UnlinkTelegram(r.Context(), u.ID); err != nil {
		if clientGone(r, err) {
			return
		}
		a.log(r).Error("unlinking telegram failed", "error", err)
		writeCode(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	a.event(r, "telegram_unlinked", "user_id", u.ID)
	w.WriteHeader(http.StatusNoContent)
}

// parsePaging reads page and page_size with the same rules as the job list.
func parsePaging(q url.Values) (page, size int, err error) {
	f, err := parseFilter(url.Values{"page": q["page"], "page_size": q["page_size"]})
	if err != nil {
		return 0, 0, err
	}
	return f.Page, f.PageSize, nil
}

// getSavedJob serves the detail of a job that is no longer publicly listed to the
// account that saved it: closed, removed, expired or older than the age limit.
func (a *accountRoutes) getSavedJob(w http.ResponseWriter, r *http.Request, id string) (job.Job, bool) {
	u, _, err := a.userOf(r)
	if err != nil {
		return job.Job{}, false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return job.Job{}, false
	}
	saved, err := a.accounts.HasSaved(r.Context(), u.ID, n)
	if err != nil || !saved {
		return job.Job{}, false
	}
	j, err := a.saved.GetForOwner(r.Context(), id)
	if err != nil {
		return job.Job{}, false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Add("Vary", "Cookie")
	return j, true
}

// maskedOrEmpty is the masked form of an address for a log line, or "" when it is not
// an address (nothing is logged for an input that failed validation).
func maskedOrEmpty(raw string) string {
	e, err := account.NormalizeEmail(raw)
	if err != nil {
		return ""
	}
	return account.MaskEmail(e)
}
