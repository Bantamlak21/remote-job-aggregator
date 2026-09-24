package company

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxEmployerNameRunes bounds an employer name taken from a job site. A
// real company name is far shorter; anything longer is a scraping error
// (a whole title or sentence in the name field) and must not become a
// company row.
const maxEmployerNameRunes = 200

// Registrar finds or creates the company and target that jobs from a
// multi-employer source (a job board, a keyword search) belong to. Such a
// source has no fixed list of companies, so the first job from an employer
// creates it; later jobs find it again.
type Registrar struct {
	companies *Store
	targets   *TargetStore
}

// NewRegistrar returns a Registrar over the two stores.
func NewRegistrar(companies *Store, targets *TargetStore) *Registrar {
	return &Registrar{companies: companies, targets: targets}
}

// FindTarget returns the existing target for (provider, employer) without
// creating anything; ok is false when there is none. Closing jobs an employer
// no longer lists needs the target only if it already exists: an employer
// first seen with nothing but ended postings must not become a company.
func (r *Registrar) FindTarget(ctx context.Context, provider, employer string) (t TargetCompany, ok bool, err error) {
	name := strings.Join(strings.Fields(employer), " ")
	if name == "" || strings.TrimSpace(provider) == "" || utf8.RuneCountInString(name) > maxEmployerNameRunes {
		return TargetCompany{}, false, nil
	}
	found, err := r.targets.GetByProviderAndBoard(ctx, provider, strings.ToLower(name))
	if errors.Is(err, ErrNotFound) {
		return TargetCompany{}, false, nil
	}
	if err != nil {
		return TargetCompany{}, false, err
	}
	return *found, true, nil
}

// EnsureTarget returns the target for (provider, employer), creating the
// company and the target when they do not exist yet. The company is matched
// by name the same way everywhere (case- and whitespace-insensitively), and
// the target's board id is that lower-cased name, so the two are 1:1.
//
// priority true (re-)marks the company as a priority company; false never
// clears the flag, so a source that does not know an employer is on the
// priority list cannot un-flag it.
func (r *Registrar) EnsureTarget(ctx context.Context, provider, employer string, priority bool) (TargetCompany, error) {
	name := strings.Join(strings.Fields(employer), " ")
	if name == "" {
		return TargetCompany{}, errors.New("company: registrar: employer name is required")
	}
	if utf8.RuneCountInString(name) > maxEmployerNameRunes {
		return TargetCompany{}, fmt.Errorf("company: registrar: employer name is %d characters, more than %d",
			utf8.RuneCountInString(name), maxEmployerNameRunes)
	}
	if strings.TrimSpace(provider) == "" {
		return TargetCompany{}, errors.New("company: registrar: provider is required")
	}

	c, err := r.companies.Upsert(ctx, UpsertParams{Name: name})
	if err != nil {
		return TargetCompany{}, err
	}
	if priority && !c.IsPriority {
		if err := r.companies.SetPriority(ctx, c.ID, true); err != nil {
			return TargetCompany{}, err
		}
	}
	t, err := r.targets.Upsert(ctx, TargetUpsertParams{
		CompanyID: c.ID, ATSProvider: provider, ExternalBoardID: strings.ToLower(name),
	})
	if err != nil {
		return TargetCompany{}, err
	}
	return *t, nil
}
