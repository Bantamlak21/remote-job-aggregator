package ats

import "testing"

func TestEmploymentType(t *testing.T) {
	for in, want := range map[string]string{
		"full_time": "full_time", "Full-time": "full_time", "FullTime": "full_time", "Permanent": "full_time", "Regular": "full_time",
		"Part-Time": "part_time", "PartTime": "part_time",
		"Contract": "contract", "Contractor": "contract", "Freelance": "contract", "Temporary": "contract", "Fixed term": "contract",
		"Intern": "internship", "Internship": "internship",
		"": "", "Other": "", "Volunteer": "",
	} {
		if got := EmploymentType(in); got != want {
			t.Errorf("EmploymentType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWorkplaceType(t *testing.T) {
	for in, want := range map[string]string{
		"remote": "remote", "Remote": "remote", "hybrid": "hybrid", "Hybrid": "hybrid",
		"on-site": "onsite", "OnSite": "onsite", "onsite": "onsite", "In office": "onsite",
		"": "", "unspecified": "", "flexible": "",
	} {
		if got := WorkplaceType(in); got != want {
			t.Errorf("WorkplaceType(%q) = %q, want %q", in, got, want)
		}
	}
}
