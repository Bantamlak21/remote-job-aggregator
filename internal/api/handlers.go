package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

// jobSummaryDTO is the wire shape of one job in a list response, and
// (embedded) the shared fields of a single-job response — see
// docs/api.md. CompanyLogoURL is a pointer so an empty Job.CompanyLogoURL
// serializes as JSON null, not "", matching the documented contract.
type jobSummaryDTO struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	CompanyName    string   `json:"company_name"`
	CompanyLogoURL *string  `json:"company_logo_url"`
	IsPriority     bool     `json:"is_priority"`
	Market         string   `json:"market"`
	Source         string   `json:"source"`
	RemoteType     string   `json:"remote_type"`
	EmploymentType string   `json:"employment_type"`
	RegionNote     string   `json:"region_note"`
	Tags           []string `json:"tags"`
	PostedAt       string   `json:"posted_at"`
	ApplicationURL string   `json:"application_url"`
	// Eligibility and Role are null until the job has been classified.
	Eligibility *eligibilityDTO `json:"eligibility"`
	Role        *roleDTO        `json:"role"`
}

// eligibilityDTO is the verdict on whether a candidate in Ethiopia could take
// the job. The whole object is null for a job that has not been classified yet.
type eligibilityDTO struct {
	Status          string   `json:"status"` // eligible | ineligible | uncertain
	Confidence      float64  `json:"confidence"`
	Basis           string   `json:"basis"`
	Restrictions    []string `json:"restrictions"`
	HoursConstraint *string  `json:"hours_constraint"`
}

// eligibilityDetailDTO adds what a single-job response carries: why, and the
// text the verdict rests on.
type eligibilityDetailDTO struct {
	eligibilityDTO
	Reasons           []string      `json:"reasons"`
	Evidence          []evidenceDTO `json:"evidence"`
	Locations         []string      `json:"locations"`
	ClassifierVersion int           `json:"classifier_version"`
}

type evidenceDTO struct {
	Field string `json:"field"`
	Text  string `json:"text"`
}

// roleDTO is what kind of role the job is (from its title).
type roleDTO struct {
	Family   string `json:"family"`
	Relevant bool   `json:"relevant"`
}

type jobDetailDTO struct {
	jobSummaryDTO
	Description string `json:"description"`
	// Eligibility shadows the summary's: the detail carries the reasons.
	Eligibility *eligibilityDetailDTO `json:"eligibility"`
}

type listJobsResponse struct {
	Jobs     []jobSummaryDTO `json:"jobs"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
	Total    int             `json:"total"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func toSummaryDTO(j job.Job) jobSummaryDTO {
	var logoURL *string
	if j.CompanyLogoURL != "" {
		logoURL = &j.CompanyLogoURL
	}
	return jobSummaryDTO{
		ID:             j.ID,
		Title:          j.Title,
		CompanyName:    j.CompanyName,
		CompanyLogoURL: logoURL,
		IsPriority:     j.IsPriority,
		Market:         string(j.Market),
		Source:         j.Source,
		RemoteType:     string(j.RemoteType),
		EmploymentType: string(j.EmploymentType),
		RegionNote:     j.RegionNote,
		Tags:           j.Tags,
		PostedAt:       j.PostedAt.Format(time.RFC3339),
		ApplicationURL: j.ApplicationURL,
		Eligibility:    toEligibilityDTO(j.Eligibility),
		Role:           toRoleDTO(j.Role),
	}
}

func toEligibilityDTO(e *job.Eligibility) *eligibilityDTO {
	if e == nil {
		return nil
	}
	dto := &eligibilityDTO{Status: e.Status, Confidence: e.Confidence, Basis: e.Basis, Restrictions: nonNil(e.Restrictions)}
	if e.HoursConstraint != "" {
		h := e.HoursConstraint
		dto.HoursConstraint = &h
	}
	return dto
}

func toRoleDTO(r *job.Role) *roleDTO {
	if r == nil {
		return nil
	}
	return &roleDTO{Family: r.Family, Relevant: r.Relevant}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func toDetailDTO(j job.Job) jobDetailDTO {
	d := jobDetailDTO{jobSummaryDTO: toSummaryDTO(j), Description: j.Description}
	if e := j.Eligibility; e != nil {
		det := &eligibilityDetailDTO{eligibilityDTO: *toEligibilityDTO(e), Reasons: nonNil(e.Reasons), Locations: nonNil(e.Locations),
			Evidence: []evidenceDTO{}, ClassifierVersion: e.ClassifierVersion}
		for _, ev := range e.Evidence {
			det.Evidence = append(det.Evidence, evidenceDTO{Field: ev.Field, Text: ev.Text})
		}
		d.Eligibility = det
	}
	return d
}

// listJobsHandler handles GET /api/v1/jobs.
func listJobsHandler(repo job.Repository, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		filter, err := parseFilter(r.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		result, err := repo.List(r.Context(), filter)
		if err != nil {
			logger.Error("listing jobs failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		dtos := make([]jobSummaryDTO, 0, len(result.Jobs))
		for _, j := range result.Jobs {
			dtos = append(dtos, toSummaryDTO(j))
		}
		writeJSON(w, http.StatusOK, listJobsResponse{
			Jobs:     dtos,
			Page:     filter.Page,
			PageSize: filter.PageSize,
			Total:    result.Total,
		})
	}
}

// getJobHandler handles GET /api/v1/jobs/{id}.
func getJobHandler(repo job.Repository, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		j, err := repo.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, job.ErrNotFound) {
				writeError(w, http.StatusNotFound, "job not found")
				return
			}
			logger.Error("getting job failed", "error", err, "id", id)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, toDetailDTO(j))
	}
}

// parseFilter turns validated query parameters into a job.Filter, with
// page/page_size always resolved to a concrete positive value (never
// left at 0 for the caller to guess a default) so the response's
// "page"/"page_size" fields always reflect what was actually applied.
// Any parameter present but invalid is a 400 — never silently ignored
// or clamped, so a typo'd remote_type doesn't quietly return every job
// scoped mistakenly.
var roleFamilyRe = regexp.MustCompile(`^[a-z_]{1,40}$`)

func parseFilter(q url.Values) (job.Filter, error) {
	filter := job.Filter{
		Query:    strings.TrimSpace(q.Get("q")),
		Company:  strings.TrimSpace(q.Get("company")),
		Tag:      strings.TrimSpace(q.Get("tag")),
		Page:     job.DefaultPage,
		PageSize: job.DefaultPageSize,
	}

	if v := q.Get("remote_type"); v != "" {
		rt := job.RemoteType(v)
		if !rt.Valid() {
			return job.Filter{}, fmt.Errorf(
				"remote_type must be one of %s, %s, %s, %s (got %q)",
				job.RemoteTypeRemote, job.RemoteTypeHybrid, job.RemoteTypeOnsite, job.RemoteTypeUnknown, v)
		}
		filter.RemoteType = rt
	}

	if v := q.Get("employment_type"); v != "" {
		et := job.EmploymentType(v)
		if !et.Valid() {
			return job.Filter{}, fmt.Errorf(
				"employment_type must be one of %s, %s, %s, %s, %s (got %q)",
				job.EmploymentTypeFullTime, job.EmploymentTypePartTime, job.EmploymentTypeContract, job.EmploymentTypeInternship, job.EmploymentTypeUnknown, v)
		}
		filter.EmploymentType = et
	}

	// "priority=true" narrows to priority companies' jobs; "priority=false"
	// is the same as omitting the parameter (it does NOT mean "only
	// non-priority"), so a UI toggle can send its state verbatim.
	if v := q.Get("priority"); v != "" {
		switch v {
		case "true":
			filter.PriorityOnly = true
		case "false":
		default:
			return job.Filter{}, fmt.Errorf("priority must be true or false (got %q)", v)
		}
	}

	// "market" selects one of the two lists; omitted means both.
	if v := q.Get("market"); v != "" {
		m := market.Market(v)
		if !m.Valid() {
			return job.Filter{}, fmt.Errorf("market must be %s or %s (got %q)", market.Ethiopia, market.Worldwide, v)
		}
		filter.Market = m
	}

	// "eligibility" is a comma-separated list of statuses to keep (eligible,
	// ineligible, uncertain, unclassified); a job matching any of them stays.
	if v := q.Get("eligibility"); v != "" {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if !job.ValidEligibilityFilter(part) {
				return job.Filter{}, fmt.Errorf("eligibility must be a comma-separated list of %s, %s, %s or %s (got %q)",
					job.EligibilityEligible, job.EligibilityIneligible, job.EligibilityUncertain, job.EligibilityUnclassified, part)
			}
			if !slices.Contains(filter.Eligibility, part) {
				filter.Eligibility = append(filter.Eligibility, part)
			}
		}
	}

	// "relevant=true" keeps roles the job seeker's profile counts as relevant;
	// "relevant=false" is the same as omitting it (like priority).
	if v := q.Get("relevant"); v != "" {
		switch v {
		case "true":
			filter.RelevantOnly = true
		case "false":
		default:
			return job.Filter{}, fmt.Errorf("relevant must be true or false (got %q)", v)
		}
	}

	if v := q.Get("role_family"); v != "" {
		if !roleFamilyRe.MatchString(v) {
			return job.Filter{}, fmt.Errorf("role_family must be lower-case letters and underscores (got %q)", v)
		}
		filter.RoleFamily = v
	}

	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return job.Filter{}, fmt.Errorf("page must be a positive integer (got %q)", v)
		}
		filter.Page = n
	}

	if v := q.Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > job.MaxPageSize {
			return job.Filter{}, fmt.Errorf("page_size must be between 1 and %d (got %q)", job.MaxPageSize, v)
		}
		filter.PageSize = n
	}

	return filter, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}
