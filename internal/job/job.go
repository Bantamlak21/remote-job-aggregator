// Package job defines the normalized job posting type the public API
// serves, and the Repository interface internal/api depends on to fetch
// it. Phase 3 (ATS ingestion) has not shipped yet, so the only
// implementation today is MockRepository (mock.go) — deterministic
// fixture data, not scraped or persisted. internal/api never imports a
// concrete repository type, only this interface, so swapping in a real
// Postgres-backed implementation later is the entire migration: no
// handler, routing, or frontend change required.
package job

import (
	"context"
	"errors"
	"time"

	"github.com/Bantamlak12/remote-job-aggregator/internal/market"
)

// RemoteType is how much of the role is done outside an office.
//
// Values match the jobs table's remote_type CHECK constraint exactly
// (migrations/000001_init_schema.up.sql) — this is not a free choice:
// Postgres rejects any other string outright. RemoteTypeUnknown exists
// because Phase 3 ingestion (Greenhouse's public Job Board API) has no
// field indicating remote/hybrid/onsite at all; classifying that from
// free-text location data is Phase 4's job (geographic eligibility &
// relevance filtering), not ingestion's — every job Phase 3 ingests is
// unknown until Phase 4 classifies it. Found and fixed before Phase 3
// began: this type originally used "fully_remote", which the database
// would have rejected on every real insert.
type RemoteType string

const (
	RemoteTypeRemote  RemoteType = "remote"
	RemoteTypeHybrid  RemoteType = "hybrid"
	RemoteTypeOnsite  RemoteType = "onsite"
	RemoteTypeUnknown RemoteType = "unknown"
)

// Valid reports whether r is one of the enumerated values. The empty
// string is deliberately not valid here — "no filter" is represented by
// omitting the field from a Filter, not by an empty RemoteType, so a
// caller that means "unset" should leave the Filter field zero rather
// than pass RemoteType("").
func (r RemoteType) Valid() bool {
	switch r {
	case RemoteTypeRemote, RemoteTypeHybrid, RemoteTypeOnsite, RemoteTypeUnknown:
		return true
	default:
		return false
	}
}

// EmploymentType is the contractual shape of the role. Values match the
// jobs table's employment_type CHECK constraint exactly.
// EmploymentTypeUnknown exists for the same reason RemoteTypeUnknown
// does: Greenhouse's public API has no employment-type field, so every
// Phase 3-ingested job starts unknown.
type EmploymentType string

const (
	EmploymentTypeFullTime   EmploymentType = "full_time"
	EmploymentTypePartTime   EmploymentType = "part_time"
	EmploymentTypeContract   EmploymentType = "contract"
	EmploymentTypeInternship EmploymentType = "internship"
	EmploymentTypeUnknown    EmploymentType = "unknown"
)

// Valid reports whether e is one of the enumerated values.
func (e EmploymentType) Valid() bool {
	switch e {
	case EmploymentTypeFullTime, EmploymentTypePartTime, EmploymentTypeContract, EmploymentTypeInternship, EmploymentTypeUnknown:
		return true
	default:
		return false
	}
}

// Job is one normalized job posting, per docs/api.md's JobDetail shape.
type Job struct {
	ID             string
	Title          string
	CompanyName    string
	CompanyLogoURL string // empty means the API serves JSON null
	// IsPriority is true for jobs from a curated priority company (the
	// Ethiopian tech companies list): the UI badges them and List can
	// filter to them. It does not change sort order (recency only).
	IsPriority bool
	// Source is the ats_provider the job was collected through ("greenhouse",
	// "remotive", "ethiojobs", ...). The UI credits job boards with it.
	Source string
	// Market is the list the job belongs to: Ethiopia or Worldwide.
	Market         market.Market
	RemoteType     RemoteType
	EmploymentType EmploymentType
	RegionNote     string
	Tags           []string
	PostedAt       time.Time
	ApplicationURL string
	Description    string
	// Eligibility and Role are nil until the job has been classified.
	Eligibility *Eligibility
	Role        *Role
}

// Eligibility is the stored verdict on whether a candidate in Ethiopia could
// take a job (see internal/filtering). A job that has not been classified yet
// has no Eligibility at all (nil), which is different from "uncertain".
type Eligibility struct {
	Status          string   // "eligible", "ineligible" or "uncertain"
	Confidence      float64  // 0..1
	Basis           string   // what the verdict rests on: "local_ethiopia", "worldwide", ...
	Restrictions    []string // limits found: "United States only"
	HoursConstraint string   // "" unless an eligible job asks for far-off working hours
	// The rest is filled by Get only.
	Reasons           []string
	Evidence          []EligibilityEvidence
	Locations         []string
	ClassifierVersion int
}

// EligibilityEvidence is a piece of the posting a verdict rests on.
type EligibilityEvidence struct {
	Field string // "location", "description", "title", "market"
	Text  string
}

// Role is what kind of role a job is, from its title.
type Role struct {
	Family   string // "software_engineering", "data_ml", "devops_security", "product_design", "it_support", "other_tech", "non_tech"
	Relevant bool   // whether the job seeker's profile counts the family as relevant
}

// Eligibility statuses, and the filter value for a job with no verdict yet.
const (
	EligibilityEligible     = "eligible"
	EligibilityIneligible   = "ineligible"
	EligibilityUncertain    = "uncertain"
	EligibilityUnclassified = "unclassified"
)

// ValidEligibilityFilter reports whether s is a status a Filter may ask for.
func ValidEligibilityFilter(s string) bool {
	switch s {
	case EligibilityEligible, EligibilityIneligible, EligibilityUncertain, EligibilityUnclassified:
		return true
	}
	return false
}

// Pagination defaults and limits, shared between internal/api (which
// resolves them before constructing a Filter, and reports the
// resolved values in its JSON response) and MockRepository's own
// defensive defaulting for callers that skip the API layer entirely
// (e.g. this package's own tests). One definition, not two hardcoded
// copies that could drift apart.
const (
	DefaultPage     = 1
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// Filter selects and paginates a List call. The zero value of each
// field except Page/PageSize means "no filter on this field" — RemoteType
// and EmploymentType are validated (via .Valid()) by internal/api before
// a Filter is ever constructed, so Repository implementations trust the
// values they're given rather than re-validating enums themselves; the
// API boundary is where untrusted input becomes a typed, checked value.
type Filter struct {
	Query          string
	RemoteType     RemoteType
	EmploymentType EmploymentType
	Company        string
	Tag            string
	PriorityOnly   bool          // true: only jobs from priority companies
	Market         market.Market // "" means both lists
	// Eligibility keeps only jobs whose verdict is one of these statuses
	// ("unclassified" matches jobs with none); empty means no filter.
	Eligibility  []string
	RelevantOnly bool   // true: only jobs whose role family is relevant
	RoleFamily   string // "" means any family
	Page         int    // <= 0 means "use the default" (1)
	PageSize     int    // <= 0 means "use the default" (20)
}

// ListResult is one page of List's matches, plus the total count across
// all pages (before pagination is applied) so a caller can compute
// page count / show "137 results" without a second call.
type ListResult struct {
	Jobs  []Job
	Total int
}

// ErrNotFound is returned by Get when no job has the given id.
var ErrNotFound = errors.New("job: not found")

// Repository is what internal/api needs from a job data source. Defined
// on the consumer side (internal/api), re-exported here for
// MockRepository to implement, matching this project's existing
// consumer-defines-the-interface convention (see internal/discovery's
// CompanyUpserter/TargetUpserter/SearchClient).
type Repository interface {
	List(ctx context.Context, filter Filter) (ListResult, error)
	Get(ctx context.Context, id string) (Job, error)
}
