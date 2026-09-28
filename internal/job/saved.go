package job

import (
	"context"
	"fmt"
	"strconv"
)

// ListByIDs returns the jobs with these ids in any state, with Closed set for the
// ones no longer open, in no particular order (the caller has its own order). It is
// for an account's saved list: an id that no longer exists is simply absent.
func (s *Store) ListByIDs(ctx context.Context, ids []int64) ([]Job, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+jobColumnsForAPI+`, `+s.closedExpr()+listFromClause+` WHERE j.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("job: listing by ids: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var (
			j              Job
			dbID           int64
			remoteType     string
			employmentType string
			locationRaw    *string
			el             eligibilityCols
		)
		if err := rows.Scan(
			&dbID, &j.Title, &j.CompanyName, &j.IsPriority, &j.Market, &j.Source, &remoteType, &employmentType,
			&locationRaw, &j.PostedAt, &j.ApplicationURL, &el.status, &el.confidence, &el.basis, &el.restrictions, &el.hours, &el.family, &el.relevant,
			&j.Closed,
		); err != nil {
			return nil, fmt.Errorf("job: scanning a saved job: %w", err)
		}
		j.ID = strconv.FormatInt(dbID, 10)
		j.RemoteType = RemoteType(remoteType)
		j.EmploymentType = EmploymentType(employmentType)
		if locationRaw != nil {
			j.RegionNote = *locationRaw
		}
		j.Tags = []string{}
		j.Eligibility, j.Role = el.build()
		out = append(out, j)
	}
	return out, rows.Err()
}
