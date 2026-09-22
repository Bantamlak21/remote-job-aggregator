package discovery

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadCompanyNames reads a plain-text, newline-delimited list of company
// names from path — one name per line, blank lines and lines starting
// with "#" ignored (a lightweight comment convention, not a format
// requirement). This is the input to CandidatesFromSearch: a list of
// names is far lighter to hand-curate than configs/seed_companies.json's
// full board_url/ats_provider/external_board_id records, since search
// finds those details instead of a human typing them in.
func LoadCompanyNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("discovery: opening company names file %s: %w", path, err)
	}
	defer f.Close()

	var names []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key := strings.ToLower(line)
		if seen[key] {
			continue // silently dedup: the same name twice costs a wasted search query, not a correctness problem
		}
		seen[key] = true
		names = append(names, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("discovery: reading company names file %s: %w", path, err)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("discovery: %s: %w", path, ErrNoCompanyNames)
	}
	return names, nil
}

// ErrNoCompanyNames is returned when a company names file parses
// cleanly but contains no usable names (empty, or only blank/comment
// lines) — distinguished from a missing/unreadable file so a caller can
// tell "you forgot to fill this in" from "this doesn't exist".
var ErrNoCompanyNames = errors.New("no company names found")
