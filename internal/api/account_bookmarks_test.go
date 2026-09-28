package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/account"
)

func hours(n int) *time.Duration { d := time.Duration(n) * time.Hour; return &d }

func TestBookmarks_SaveListAndRemove(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("saver@example.com")
	a, b := seedJob(t, e.pool, seedJobSpec{}), seedJob(t, e.pool, seedJobSpec{})

	for _, id := range []int64{a, a, b} { // saving twice is fine
		if rec := e.do("PUT", bookmarkPath(id), "", withCookie(c)); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("save %d = %d %q, want 204", id, rec.Code, rec.Body)
		}
	}
	ids := decode[map[string][]string](t, e.do("GET", "/api/v1/me/bookmarks/ids", "", withCookie(c)))
	if got := ids["ids"]; len(got) != 2 || got[0] != idStr(b) || got[1] != idStr(a) {
		t.Errorf("ids = %v, want [%d %d] newest first", got, b, a)
	}

	rec := e.do("GET", "/api/v1/me/bookmarks", "", withCookie(c))
	list := decode[savedListResponse](t, rec)
	if list.Total != 2 || list.Page != 1 || list.PageSize != 20 || len(list.Jobs) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if list.Jobs[0].ID != idStr(b) || list.Jobs[1].ID != idStr(a) {
		t.Errorf("order = %v, want newest saved first", jobIDs(list))
	}
	for _, j := range list.Jobs {
		if j.Closed {
			t.Errorf("open job %s reported closed", j.ID)
		}
		if _, err := time.Parse(time.RFC3339, j.SavedAt); err != nil {
			t.Errorf("saved_at = %q: %v", j.SavedAt, err)
		}
		if j.Title == "" || j.CompanyName == "" || j.ApplicationURL == "" || j.PostedAt == "" {
			t.Errorf("a saved job lacks its summary fields: %+v", j)
		}
	}

	for i := 0; i < 2; i++ { // removing twice is fine
		if rec := e.do("DELETE", bookmarkPath(a), "", withCookie(c)); rec.Code != http.StatusNoContent {
			t.Fatalf("unsave = %d", rec.Code)
		}
	}
	list = decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks", "", withCookie(c)))
	if list.Total != 1 || list.Jobs[0].ID != idStr(b) {
		t.Errorf("after unsave: %v total %d", jobIDs(list), list.Total)
	}
}

func TestBookmarks_Paging(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("pager@example.com")
	var ids []int64
	for i := 0; i < 3; i++ {
		id := seedJob(t, e.pool, seedJobSpec{})
		ids = append(ids, id)
		e.do("PUT", bookmarkPath(id), "", withCookie(c))
		time.Sleep(2 * time.Millisecond)
	}
	p1 := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks?page_size=2", "", withCookie(c)))
	p2 := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks?page_size=2&page=2", "", withCookie(c)))
	p3 := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks?page_size=2&page=9", "", withCookie(c)))
	if len(p1.Jobs) != 2 || len(p2.Jobs) != 1 || len(p3.Jobs) != 0 || p1.Total != 3 || p2.Total != 3 || p3.Total != 3 {
		t.Errorf("pages = %v / %v / %v, totals %d %d %d", jobIDs(p1), jobIDs(p2), jobIDs(p3), p1.Total, p2.Total, p3.Total)
	}
	if p1.Jobs[0].ID != idStr(ids[2]) || p2.Jobs[0].ID != idStr(ids[0]) || p2.Page != 2 || p2.PageSize != 2 {
		t.Errorf("paging order/echo wrong: %v %v page=%d size=%d", jobIDs(p1), jobIDs(p2), p2.Page, p2.PageSize)
	}
	for _, q := range []string{"page=0", "page=-1", "page=x", "page_size=0", "page_size=abc", "page_size=100000"} {
		if rec := e.do("GET", "/api/v1/me/bookmarks?"+q, "", withCookie(c)); rec.Code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, rec.Code)
		}
	}
	if list := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks", "", withCookie(e.signIn("empty@example.com")))); list.Jobs == nil || len(list.Jobs) != 0 || list.Total != 0 {
		t.Errorf("an empty list = %+v, want jobs [] (not null) and total 0", list)
	}
}

func TestBookmarks_OnlyOpenExistingJobsCanBeSaved(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("picky@example.com")
	closed := seedJob(t, e.pool, seedJobSpec{status: "closed"})
	removed := seedJob(t, e.pool, seedJobSpec{status: "removed"})
	for name, path := range map[string]string{
		"closed": bookmarkPath(closed), "removed": bookmarkPath(removed), "missing": bookmarkPath(987654321),
		"zero": "/api/v1/me/bookmarks/0", "negative": "/api/v1/me/bookmarks/-5", "not a number": "/api/v1/me/bookmarks/abc",
		"sql": "/api/v1/me/bookmarks/1%20OR%201=1", "overflow": "/api/v1/me/bookmarks/99999999999999999999999", "float": "/api/v1/me/bookmarks/1.5",
	} {
		rec := e.do("PUT", path, "", withCookie(c))
		if rec.Code != http.StatusNotFound {
			t.Errorf("save (%s) = %d, want 404", name, rec.Code)
		}
	}
	if n := count(t, e.pool, `SELECT count(*) FROM bookmarks`); n != 0 {
		t.Errorf("bookmarks = %d, want 0", n)
	}
	for _, path := range []string{"/api/v1/me/bookmarks/abc", "/api/v1/me/bookmarks/0", "/api/v1/me/bookmarks/99999999999999999999999"} {
		if rec := e.do("DELETE", path, "", withCookie(c)); rec.Code != http.StatusNoContent {
			t.Errorf("DELETE %s = %d, want 204 (nothing to remove)", path, rec.Code)
		}
	}
}

func TestBookmarks_LimitIs409AndResavingStillWorks(t *testing.T) {
	e := newEnv(t, envOpts{tune: func(c *account.Config) { c.MaxBookmarks = 2 }})
	c := e.signIn("limit@example.com")
	a, b, third := seedJob(t, e.pool, seedJobSpec{}), seedJob(t, e.pool, seedJobSpec{}), seedJob(t, e.pool, seedJobSpec{})
	for _, id := range []int64{a, b} {
		if rec := e.do("PUT", bookmarkPath(id), "", withCookie(c)); rec.Code != http.StatusNoContent {
			t.Fatalf("save = %d", rec.Code)
		}
	}
	rec := e.do("PUT", bookmarkPath(third), "", withCookie(c))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "limit") {
		t.Errorf("save past the limit = %d %s, want 409 with a message about the limit", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", bookmarkPath(a), "", withCookie(c)); rec.Code != http.StatusNoContent {
		t.Errorf("re-saving at the limit = %d, want 204", rec.Code)
	}
	e.do("DELETE", bookmarkPath(a), "", withCookie(c))
	if rec := e.do("PUT", bookmarkPath(third), "", withCookie(c)); rec.Code != http.StatusNoContent {
		t.Errorf("save after freeing a slot = %d, want 204", rec.Code)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM bookmarks`); n != 2 {
		t.Errorf("bookmarks = %d, want 2", n)
	}
}

func TestBookmarks_ConcurrentSavesNeverPassTheLimit(t *testing.T) {
	e := newEnv(t, envOpts{tune: func(c *account.Config) { c.MaxBookmarks = 5 }})
	c := e.signIn("race@example.com")
	const jobs = 20
	ids := make([]int64, jobs)
	for i := range ids {
		ids[i] = seedJob(t, e.pool, seedJobSpec{})
	}
	codes := make(chan int, jobs)
	start := make(chan struct{})
	for _, id := range ids {
		go func() {
			<-start
			// httptest recorders and the handler are safe to use concurrently; e.do records under a lock.
			codes <- e.do("PUT", bookmarkPath(id), "", withCookie(c)).Code
		}()
	}
	close(start)
	got := map[int]int{}
	for i := 0; i < jobs; i++ {
		got[<-codes]++
	}
	if got[204] != 5 || got[409] != jobs-5 {
		t.Errorf("status counts = %v, want 5 x 204 and %d x 409", got, jobs-5)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM bookmarks`); n != 5 {
		t.Errorf("bookmarks = %d, want 5", n)
	}
}

// ---- other people's data ---------------------------------------------------------

func TestBookmarks_OneUserCannotSeeOrRemoveAnothers(t *testing.T) {
	e := newEnv(t, envOpts{})
	a, b := e.signIn("alice@example.com"), e.signIn("bob@example.com")
	aliceUser := decode[userResponse](t, e.do("GET", "/api/v1/me", "", withCookie(a)))
	jobX, jobY := seedJob(t, e.pool, seedJobSpec{}), seedJob(t, e.pool, seedJobSpec{})
	e.do("PUT", bookmarkPath(jobX), "", withCookie(a))

	// Bob sees none of it, however he asks.
	list := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks", "", withCookie(b)))
	if list.Total != 0 || len(list.Jobs) != 0 {
		t.Errorf("bob's list = %v (total %d), want empty", jobIDs(list), list.Total)
	}
	for _, q := range []string{"?user_id=" + aliceUser.User.ID, "?userId=" + aliceUser.User.ID, "?email=alice@example.com", "?user=" + aliceUser.User.ID} {
		list := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks"+q, "", withCookie(b)))
		if list.Total != 0 || len(list.Jobs) != 0 {
			t.Errorf("bob asking with %s got %v: the account comes from the session only", q, jobIDs(list))
		}
	}
	if ids := decode[map[string][]string](t, e.do("GET", "/api/v1/me/bookmarks/ids", "", withCookie(b))); len(ids["ids"]) != 0 {
		t.Errorf("bob's ids = %v", ids)
	}

	// Bob removing "his" bookmark on jobX does nothing to Alice's.
	if rec := e.do("DELETE", bookmarkPath(jobX), "", withCookie(b)); rec.Code != http.StatusNoContent {
		t.Errorf("bob's delete = %d", rec.Code)
	}
	if n := count(t, e.pool, `SELECT count(*) FROM bookmarks WHERE user_id = $1::bigint`, aliceUser.User.ID); n != 1 {
		t.Errorf("alice's bookmarks after bob's delete = %d, want 1", n)
	}

	// Bob saving the same job is his own row and leaves Alice's alone.
	if rec := e.do("PUT", bookmarkPath(jobX), "", withCookie(b)); rec.Code != http.StatusNoContent {
		t.Errorf("bob's save = %d", rec.Code)
	}
	e.do("PUT", bookmarkPath(jobY), "", withCookie(b))
	if list := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks", "", withCookie(a))); list.Total != 1 || list.Jobs[0].ID != idStr(jobX) {
		t.Errorf("alice's list after bob saved: %v", jobIDs(list))
	}
	e.do("DELETE", bookmarkPath(jobX), "", withCookie(b))
	if list := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks", "", withCookie(a))); list.Total != 1 {
		t.Errorf("alice lost her bookmark when bob removed his own: %v", jobIDs(list))
	}
}

func TestBookmarks_SavedListShowsClosedAndAgedOutJobs(t *testing.T) {
	e := newEnv(t, envOpts{maxJobAge: 15 * 24 * time.Hour})
	c := e.signIn("keeper@example.com")
	open := seedJob(t, e.pool, seedJobSpec{})
	toClose := seedJob(t, e.pool, seedJobSpec{})
	aged := seedJob(t, e.pool, seedJobSpec{}) // saved while fresh; aged below
	deadline := seedJob(t, e.pool, seedJobSpec{expiresIn: hours(5)})
	for _, id := range []int64{open, toClose, aged, deadline} {
		if rec := e.do("PUT", bookmarkPath(id), "", withCookie(c)); rec.Code != http.StatusNoContent {
			t.Fatalf("save %d = %d", id, rec.Code)
		}
		time.Sleep(2 * time.Millisecond)
	}
	exec(t, e.pool, `UPDATE jobs SET status = 'closed', closed_at = now() WHERE id = $1`, toClose)
	exec(t, e.pool, `UPDATE jobs SET expires_at = now() - interval '1 hour' WHERE id = $1`, deadline)
	exec(t, e.pool, `UPDATE jobs SET published_at = now() - interval '40 days' WHERE id = $1`, aged) // open on its board, past the age limit

	// The public list has only the open, fresh, un-expired one.
	public := decode[listJobsResponse](t, e.do("GET", "/api/v1/jobs", ""))
	if public.Total != 1 || public.Jobs[0].ID != idStr(open) {
		t.Fatalf("public list = %+v, want only job %d", public, open)
	}

	list := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks", "", withCookie(c)))
	if list.Total != 4 || len(list.Jobs) != 4 {
		t.Fatalf("saved list = %v total %d, want all 4 (a saved job never silently disappears)", jobIDs(list), list.Total)
	}
	closedByID := map[string]bool{}
	for _, j := range list.Jobs {
		closedByID[j.ID] = j.Closed
	}
	// "closed" means "the public would not see this job": that includes one past the age limit.
	want := map[int64]bool{open: false, toClose: true, aged: true, deadline: true}
	for id, wantClosed := range want {
		got, ok := closedByID[idStr(id)]
		if !ok {
			t.Errorf("job %d is missing from the saved list", id)
		} else if got != wantClosed {
			t.Errorf("job %d closed = %v, want %v", id, got, wantClosed)
		}
	}
	// Order: saved newest first.
	if list.Jobs[0].ID != idStr(deadline) || list.Jobs[3].ID != idStr(open) {
		t.Errorf("order = %v", jobIDs(list))
	}
}

func TestJobDetail_ClosedJobIsShownOnlyToTheAccountThatSavedIt(t *testing.T) {
	e := newEnv(t, envOpts{})
	owner, stranger := e.signIn("owner@example.com"), e.signIn("stranger@example.com")
	id := seedJob(t, e.pool, seedJobSpec{})
	path := "/api/v1/jobs/" + idStr(id)
	if rec := e.do("PUT", bookmarkPath(id), "", withCookie(owner)); rec.Code != 204 {
		t.Fatal(rec.Code)
	}

	// While open it is public, closed:false for everyone.
	for name, opt := range map[string]reqOpt{"signed out": func(*http.Request) {}, "owner": withCookie(owner), "stranger": withCookie(stranger)} {
		rec := e.do("GET", path, "", opt)
		if rec.Code != 200 || decode[jobDetailDTO](t, rec).Closed {
			t.Errorf("open job as %s = %d closed=%v", name, rec.Code, rec.Code == 200 && decode[jobDetailDTO](t, rec).Closed)
		}
	}

	exec(t, e.pool, `UPDATE jobs SET status = 'closed', closed_at = now() WHERE id = $1`, id)

	rec := e.do("GET", path, "", withCookie(owner))
	if rec.Code != 200 {
		t.Fatalf("the owner's view of their closed saved job = %d %s, want 200", rec.Code, rec.Body)
	}
	d := decode[jobDetailDTO](t, rec)
	if !d.Closed || d.ID != idStr(id) || d.Description == "" || d.ApplicationURL == "" {
		t.Errorf("detail = %+v, want closed:true with the full job", d)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, want private, no-store: a shared cache must not keep a per-account response", cc)
	}
	if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Cookie") {
		t.Errorf("Vary = %v, want Cookie", rec.Header().Values("Vary"))
	}

	notFound := map[string]reqOpt{
		"signed out":                    func(*http.Request) {},
		"another signed-in user":        withCookie(stranger),
		"a garbage cookie":              withCookie(&http.Cookie{Name: e.cookieName, Value: "garbage"}),
		"a well-formed fake":            withCookie(&http.Cookie{Name: e.cookieName, Value: strings.Repeat("Q", 43)}),
		"the owner's token, wrong name": withCookie(&http.Cookie{Name: "session", Value: owner.Value}),
	}
	for name, opt := range notFound {
		rec := e.do("GET", path, "", opt)
		if rec.Code != http.StatusNotFound {
			t.Errorf("a closed job as %s = %d %s, want 404", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "Engineer") || strings.Contains(rec.Body.String(), "Description") {
			t.Errorf("the 404 for %s leaks the job: %s", name, rec.Body)
		}
	}

	// Once the owner unsaves it, it is gone for them too.
	e.do("DELETE", bookmarkPath(id), "", withCookie(owner))
	if rec := e.do("GET", path, "", withCookie(owner)); rec.Code != http.StatusNotFound {
		t.Errorf("after unsaving, the owner's view = %d, want 404", rec.Code)
	}
}

func TestJobDetail_ClosedJobSavedByOthersIsNotShownToAnotherSignedInUser(t *testing.T) {
	e := newEnv(t, envOpts{})
	saver, other := e.signIn("saver@example.com"), e.signIn("other@example.com")
	id := seedJob(t, e.pool, seedJobSpec{})
	e.do("PUT", bookmarkPath(id), "", withCookie(saver))
	exec(t, e.pool, `UPDATE jobs SET status = 'removed', closed_at = now() WHERE id = $1`, id)

	if rec := e.do("GET", "/api/v1/jobs/"+idStr(id), "", withCookie(other)); rec.Code != http.StatusNotFound {
		t.Errorf("a signed-in user who did not save the job = %d, want 404", rec.Code)
	}
	if rec := e.do("GET", "/api/v1/jobs/"+idStr(id), "", withCookie(saver)); rec.Code != http.StatusOK || !decode[jobDetailDTO](t, rec).Closed {
		t.Errorf("the saver = %d, want 200 closed", rec.Code)
	}
}

func TestJobDetail_AgedOutAndExpiredJobsForTheirOwner(t *testing.T) {
	e := newEnv(t, envOpts{maxJobAge: 15 * 24 * time.Hour})
	owner, stranger := e.signIn("o@example.com"), e.signIn("s@example.com")
	aged := seedJob(t, e.pool, seedJobSpec{})
	deadline := seedJob(t, e.pool, seedJobSpec{expiresIn: hours(3)})
	for _, id := range []int64{aged, deadline} {
		if rec := e.do("PUT", bookmarkPath(id), "", withCookie(owner)); rec.Code != 204 {
			t.Fatalf("save = %d", rec.Code)
		}
	}
	exec(t, e.pool, `UPDATE jobs SET expires_at = now() - interval '1 minute' WHERE id = $1`, deadline)
	exec(t, e.pool, `UPDATE jobs SET published_at = now() - interval '40 days' WHERE id = $1`, aged)

	for name, tc := range map[string]struct {
		id         int64
		wantClosed bool
	}{"past the age limit": {aged, true}, "past its deadline": {deadline, true}} {
		path := "/api/v1/jobs/" + idStr(tc.id)
		if rec := e.do("GET", path, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: public detail = %d, want 404", name, rec.Code)
		}
		if rec := e.do("GET", path, "", withCookie(stranger)); rec.Code != http.StatusNotFound {
			t.Errorf("%s: a stranger = %d, want 404", name, rec.Code)
		}
		rec := e.do("GET", path, "", withCookie(owner))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: the owner = %d, want 200", name, rec.Code)
			continue
		}
		if got := decode[jobDetailDTO](t, rec).Closed; got != tc.wantClosed {
			t.Errorf("%s: closed = %v, want %v", name, got, tc.wantClosed)
		}
	}
}

func TestJobDetail_ADeletedJobIsGoneForEveryone(t *testing.T) {
	e := newEnv(t, envOpts{})
	owner := e.signIn("gone-owner@example.com")
	id := seedJob(t, e.pool, seedJobSpec{})
	e.do("PUT", bookmarkPath(id), "", withCookie(owner))
	exec(t, e.pool, `DELETE FROM jobs WHERE id = $1`, id)
	if rec := e.do("GET", "/api/v1/jobs/"+idStr(id), "", withCookie(owner)); rec.Code != http.StatusNotFound {
		t.Errorf("a deleted job = %d, want 404", rec.Code)
	}
	if list := decode[savedListResponse](t, e.do("GET", "/api/v1/me/bookmarks", "", withCookie(owner))); list.Total != 0 || len(list.Jobs) != 0 {
		t.Errorf("the saved list still has the deleted job: %v", jobIDs(list))
	}
	for _, bad := range []string{"abc", "0", "-1", "1e3"} {
		if rec := e.do("GET", "/api/v1/jobs/"+bad, "", withCookie(owner)); rec.Code != http.StatusNotFound {
			t.Errorf("GET /jobs/%s = %d, want 404", bad, rec.Code)
		}
	}
}

func TestJobDetail_PublicJobsNeverChangeForSignedInUsers(t *testing.T) {
	e := newEnv(t, envOpts{})
	c := e.signIn("plain@example.com")
	id := seedJob(t, e.pool, seedJobSpec{})
	anon := e.do("GET", "/api/v1/jobs/"+idStr(id), "")
	signed := e.do("GET", "/api/v1/jobs/"+idStr(id), "", withCookie(c))
	if anon.Code != 200 || anon.Body.String() != signed.Body.String() {
		t.Errorf("an open job differs by session:\nanon:   %s\nsigned: %s", anon.Body, signed.Body)
	}
	if cc := anon.Header().Get("Cache-Control"); strings.Contains(cc, "private") {
		t.Errorf("the public detail got %q", cc)
	}
}
