// Package priority owns the curated list of priority companies (the
// Ethiopian tech companies whose jobs the board pins first): loading the
// list from configs/ethiopian_companies.json, registering each company and
// the places its jobs come from, and reporting how well those sources
// cover the list.
//
// It decides nothing about how jobs are fetched; that is the job of the
// source clients in internal/ats. It only says, per company, which
// sources to register.
package priority

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/Bantamlak12/remote-job-aggregator/internal/ats"
	"github.com/Bantamlak12/remote-job-aggregator/internal/company"
)

// Entry is one company in the list.
type Entry struct {
	// Name is the company's canonical name. It is the companies row's name
	// and the board id of the company's search target.
	Name string `json:"name"`
	// Aliases are other names the company appears under on job sites.
	Aliases []string `json:"aliases"`
	// Website is the company's site, when verified; it is not guessed.
	Website string `json:"website"`
	// HiresOutsideEthiopia marks a company that also posts jobs located in
	// other countries. Search results for any other company must show an
	// Ethiopia signal to count, since another company may share its name.
	HiresOutsideEthiopia bool `json:"hires_outside_ethiopia"`
	// Feeds are RSS feed URLs listing the company's openings.
	Feeds []string `json:"feeds"`
	// CareerPages are pages on the company's own site that link to its
	// job postings.
	CareerPages []string `json:"career_pages"`
}

// Config is the whole list.
type Config struct {
	Companies []Entry `json:"companies"`
}

// Load reads and validates the list at path.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("priority: opening %s: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields() // a typo like "carreer_pages" must not silently drop a source
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("priority: parsing %s: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("priority: parsing %s: unexpected data after the config object", path)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("priority: %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the list is usable: at least one company, every name
// present and unique (case-insensitively), every URL an absolute http(s)
// URL, and no URL registered under two companies (a feed or page can only
// belong to one, and the database would reject the second anyway).
func (c Config) Validate() error {
	if len(c.Companies) == 0 {
		return errors.New("no companies listed")
	}
	var errs []error
	names := map[string]bool{}
	urls := map[string]string{}
	for i, e := range c.Companies {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			errs = append(errs, fmt.Errorf("company #%d has no name", i+1))
			continue
		}
		if key := strings.ToLower(name); names[key] {
			errs = append(errs, fmt.Errorf("company %q is listed twice", name))
		} else {
			names[key] = true
		}
		check := func(kind, u string) {
			if !isHTTPURL(u) {
				errs = append(errs, fmt.Errorf("company %q: %s %q is not an absolute http(s) URL", name, kind, u))
				return
			}
			key := strings.ToLower(strings.TrimSpace(u))
			if other, dup := urls[key]; dup {
				errs = append(errs, fmt.Errorf("company %q: %s %q is already used by %q", name, kind, u, other))
				return
			}
			urls[key] = name
		}
		if e.Website != "" && !isHTTPURL(e.Website) {
			errs = append(errs, fmt.Errorf("company %q: website %q is not an absolute http(s) URL", name, e.Website))
		}
		for _, u := range e.Feeds {
			check("feed", u)
		}
		for _, u := range e.CareerPages {
			check("career page", u)
		}
	}
	return errors.Join(errs...)
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// CompanyStore is what Seed needs from internal/company's company Store.
type CompanyStore interface {
	Upsert(ctx context.Context, params company.UpsertParams) (*company.Company, error)
	SetPriority(ctx context.Context, id int64, priority bool) error
}

// TargetStore is what Seed needs from internal/company's TargetStore.
type TargetStore interface {
	Upsert(ctx context.Context, params company.TargetUpsertParams) (*company.TargetCompany, error)
}

// SeedResult counts what Seed registered.
type SeedResult struct {
	Companies int
	Targets   int
}

// Seed registers every company in cfg as a priority company and every
// source it names as a target: one "search" target per company (its name is
// the board id), plus one target per feed and per career page. Safe to run
// repeatedly: it only inserts what is missing and re-marks companies as
// priority; it never removes a company, flips one back, or reassigns a
// source that belongs to another company.
//
// A failure on one company is collected and does not stop the others, so
// one bad entry (say, a website already claimed by another company) cannot
// leave the rest of the list unregistered. The returned error joins them.
func Seed(ctx context.Context, cfg Config, companies CompanyStore, targets TargetStore) (SeedResult, error) {
	var (
		res  SeedResult
		errs []error
	)
	for _, e := range cfg.Companies {
		if err := ctx.Err(); err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		n, err := seedOne(ctx, e, companies, targets)
		res.Targets += n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name, err))
			continue
		}
		res.Companies++
	}
	return res, errors.Join(errs...)
}

func seedOne(ctx context.Context, e Entry, companies CompanyStore, targets TargetStore) (int, error) {
	c, err := companies.Upsert(ctx, company.UpsertParams{Name: e.Name, Website: e.Website})
	if err != nil {
		return 0, err
	}
	if err := companies.SetPriority(ctx, c.ID, true); err != nil {
		return 0, err
	}

	type source struct {
		provider ats.Provider
		board    string
		url      string
	}
	sources := []source{{ats.ProviderSearch, strings.TrimSpace(e.Name), ""}}
	for _, u := range e.Feeds {
		sources = append(sources, source{ats.ProviderFeed, strings.TrimSpace(u), strings.TrimSpace(u)})
	}
	for _, u := range e.CareerPages {
		sources = append(sources, source{ats.ProviderCareersSite, strings.TrimSpace(u), strings.TrimSpace(u)})
	}

	registered := 0
	var errs []error
	for _, s := range sources {
		if _, err := targets.Upsert(ctx, company.TargetUpsertParams{
			CompanyID: c.ID, ATSProvider: string(s.provider), ExternalBoardID: s.board, BoardURL: s.url,
		}); err != nil {
			errs = append(errs, fmt.Errorf("registering %s target %q: %w", s.provider, s.board, err))
			continue
		}
		registered++
	}
	return registered, errors.Join(errs...)
}
