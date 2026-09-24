package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/job"
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
	RemoteType     string   `json:"remote_type"`
	EmploymentType string   `json:"employment_type"`
	RegionNote     string   `json:"region_note"`
	Tags           []string `json:"tags"`
	PostedAt       string   `json:"posted_at"`
	ApplicationURL string   `json:"application_url"`
}

type jobDetailDTO struct {
	jobSummaryDTO
	Description string `json:"description"`
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
		RemoteType:     string(j.RemoteType),
		EmploymentType: string(j.EmploymentType),
		RegionNote:     j.RegionNote,
		Tags:           j.Tags,
		PostedAt:       j.PostedAt.Format(time.RFC3339),
		ApplicationURL: j.ApplicationURL,
	}
}

func toDetailDTO(j job.Job) jobDetailDTO {
	return jobDetailDTO{jobSummaryDTO: toSummaryDTO(j), Description: j.Description}
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
