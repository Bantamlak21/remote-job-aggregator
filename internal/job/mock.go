package job

import (
	"context"
	"sort"
	"strings"
	"time"
)

// MockRepository serves a fixed set of illustrative fixture jobs — not
// scraped, not persisted, not claimed to be real postings. Each
// ApplicationURL points at the named company's real public careers/ATS
// domain (verifiable), but the specific job and description are
// synthetic, built to give the job-board frontend realistic, varied
// data to design and test against before Phase 3 ships real ingestion.
//
// Immutable after construction: List/Get never mutate the underlying
// slice, so one MockRepository is safe to share across concurrent
// requests without a lock.
type MockRepository struct {
	jobs []Job
}

// NewMockRepository returns a Repository backed by fixtureJobs().
func NewMockRepository() *MockRepository {
	return &MockRepository{jobs: fixtureJobs()}
}

// List returns the jobs matching filter, newest-first (ties broken by
// ID for a stable, testable order), paginated.
func (m *MockRepository) List(ctx context.Context, filter Filter) (ListResult, error) {
	if err := ctx.Err(); err != nil {
		return ListResult{}, err
	}

	q := strings.ToLower(strings.TrimSpace(filter.Query))
	matched := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		if filter.RemoteType != "" && j.RemoteType != filter.RemoteType {
			continue
		}
		if filter.EmploymentType != "" && j.EmploymentType != filter.EmploymentType {
			continue
		}
		if filter.Company != "" && !strings.EqualFold(j.CompanyName, filter.Company) {
			continue
		}
		if filter.Tag != "" && !hasTagFold(j.Tags, filter.Tag) {
			continue
		}
		if q != "" && !matchesQuery(j, q) {
			continue
		}
		matched = append(matched, j)
	}

	sort.SliceStable(matched, func(i, k int) bool {
		if !matched[i].PostedAt.Equal(matched[k].PostedAt) {
			return matched[i].PostedAt.After(matched[k].PostedAt)
		}
		return matched[i].ID < matched[k].ID
	})

	total := len(matched)
	page := filter.Page
	if page <= 0 {
		page = DefaultPage
	}
	pageSize := filter.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}

	start := min((page-1)*pageSize, total)
	end := min(start+pageSize, total)

	return ListResult{Jobs: append([]Job{}, matched[start:end]...), Total: total}, nil
}

// Get returns the job with the given id, or ErrNotFound.
func (m *MockRepository) Get(ctx context.Context, id string) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	for _, j := range m.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return Job{}, ErrNotFound
}

func matchesQuery(j Job, lowerQuery string) bool {
	if strings.Contains(strings.ToLower(j.Title), lowerQuery) {
		return true
	}
	if strings.Contains(strings.ToLower(j.CompanyName), lowerQuery) {
		return true
	}
	for _, t := range j.Tags {
		if strings.Contains(strings.ToLower(t), lowerQuery) {
			return true
		}
	}
	return false
}

func hasTagFold(tags []string, tag string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// fixtureJobs returns 12 illustrative jobs spanning every
// EmploymentType, fully_remote and hybrid RemoteTypes (deliberately no
// onsite fixture — a remote-job aggregator's illustrative data leans
// remote/hybrid; RemoteTypeOnsite is still a fully valid filter value,
// it simply matches zero of these fixtures today, which is a real,
// legitimate outcome, not a bug), and a mix of worldwide vs.
// region-restricted postings — enough variety for the frontend to
// exercise most filter combinations without needing a real ATS
// integration yet.
func fixtureJobs() []Job {
	return []Job{
		{
			ID: "job_spotify_backend_eng", Title: "Backend Engineer, Payments",
			CompanyName: "Spotify", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "Worldwide", Tags: []string{"go", "backend", "payments"},
			PostedAt: date(2026, 9, 22), ApplicationURL: "https://jobs.lever.co/spotify",
			Description: "Own the payments processing pipeline that powers Spotify Premium billing across 180+ markets. You'll work closely with the platform and fraud teams to keep payment success rates high while shipping new payment methods.",
		},
		{
			ID: "job_airbnb_product_designer", Title: "Product Designer, Trust & Safety",
			CompanyName: "Airbnb", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "US/Canada preferred", Tags: []string{"design", "figma", "product"},
			PostedAt: date(2026, 9, 21), ApplicationURL: "https://boards.greenhouse.io/airbnb",
			Description: "Design the flows that keep guests and hosts safe, from identity verification to dispute resolution. You'll partner with research and policy to ship changes that are measured against real trust-and-safety outcomes.",
		},
		{
			ID: "job_gitlab_sre", Title: "Site Reliability Engineer",
			CompanyName: "GitLab", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "Worldwide", Tags: []string{"sre", "kubernetes", "go"},
			PostedAt: date(2026, 9, 20), ApplicationURL: "https://about.gitlab.com/jobs/",
			Description: "Keep GitLab.com's Kubernetes-based infrastructure reliable at scale. You'll own on-call rotations, incident response, and the automation that turns repeated manual fixes into permanent guardrails.",
		},
		{
			ID: "job_automattic_support_eng", Title: "Support Engineer, WordPress.com",
			CompanyName: "Automattic", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "Worldwide", Tags: []string{"support", "wordpress", "customer"},
			PostedAt: date(2026, 9, 18), ApplicationURL: "https://automattic.com/work-with-us/",
			Description: "Solve real technical problems for WordPress.com customers over chat and email — theme issues, plugin conflicts, performance debugging. Every Automattician starts in support, including engineers and executives.",
		},
		{
			ID: "job_zapier_data_analyst", Title: "Data Analyst",
			CompanyName: "Zapier", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "US timezones", Tags: []string{"sql", "analytics", "data"},
			PostedAt: date(2026, 9, 17), ApplicationURL: "https://zapier.com/jobs",
			Description: "Turn product and growth questions into SQL queries and dashboards the whole company trusts. You'll partner with product managers to define the metrics that decide what Zapier builds next.",
		},
		{
			ID: "job_doist_android_eng", Title: "Mobile Engineer, Android",
			CompanyName: "Doist", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeContract,
			RegionNote: "Worldwide", Tags: []string{"android", "kotlin", "mobile"},
			PostedAt: date(2026, 9, 15), ApplicationURL: "https://doist.com/careers",
			Description: "Ship features for Todoist's Android app, used by millions of people to manage their day. You'll work in a small, async-first team that values calm, sustainable software over crunch.",
		},
		{
			ID: "job_toptal_recruiter", Title: "Technical Recruiter",
			CompanyName: "Toptal", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypePartTime,
			RegionNote: "Worldwide", Tags: []string{"recruiting", "hr", "talent"},
			PostedAt: date(2026, 9, 14), ApplicationURL: "https://www.toptal.com/careers",
			Description: "Source and screen elite freelance engineers and designers for Toptal's network. You'll run technical phone screens and work closely with talent operations to keep acceptance rates high.",
		},
		{
			ID: "job_buffer_content_marketing", Title: "Content Marketing Manager",
			CompanyName: "Buffer", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "Worldwide", Tags: []string{"marketing", "content", "seo"},
			PostedAt: date(2026, 9, 12), ApplicationURL: "https://buffer.com/journey",
			Description: "Plan and write the long-form content that drives Buffer's organic growth. You'll own the editorial calendar end to end, from keyword research to publishing and performance review.",
		},
		{
			ID: "job_remote_payroll_specialist", Title: "Payroll Specialist, EMEA",
			CompanyName: "Remote.com", RemoteType: RemoteTypeHybrid, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "Lisbon or remote in EU", Tags: []string{"payroll", "hr", "emea"},
			PostedAt: date(2026, 9, 10), ApplicationURL: "https://remote.com/careers",
			Description: "Process payroll for Remote's EMEA customers across a dozen countries, each with its own statutory rules. You'll be the point of escalation when a local payroll run doesn't go as planned.",
		},
		{
			ID: "job_basecamp_support_rep", Title: "Customer Support Representative",
			CompanyName: "Basecamp", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "US/EU timezones", Tags: []string{"support", "saas"},
			PostedAt: date(2026, 9, 8), ApplicationURL: "https://basecamp.com/about/jobs",
			Description: "Answer Basecamp customers' questions directly, in your own words, with no scripts. You'll also flag the patterns support sees that the product team should know about.",
		},
		{
			ID: "job_duckduckgo_backend_eng", Title: "Backend Engineer, Search",
			CompanyName: "DuckDuckGo", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeFullTime,
			RegionNote: "Worldwide", Tags: []string{"privacy", "go", "backend"},
			PostedAt: date(2026, 9, 5), ApplicationURL: "https://duckduckgo.com/hiring",
			Description: "Build search infrastructure that never tracks the person using it. You'll work on ranking, query understanding, and the infrastructure that serves it all with strict privacy guarantees.",
		},
		{
			ID: "job_invision_ux_researcher", Title: "UX Researcher (Internship)",
			CompanyName: "InVision", RemoteType: RemoteTypeFullyRemote, EmploymentType: EmploymentTypeInternship,
			RegionNote: "Worldwide", Tags: []string{"research", "ux", "design"},
			PostedAt: date(2026, 9, 3), ApplicationURL: "https://www.invisionapp.com/company/careers",
			Description: "Run usability studies on InVision's design collaboration tools and turn findings into concrete recommendations for the product team. A structured, mentored first research role.",
		},
	}
}
