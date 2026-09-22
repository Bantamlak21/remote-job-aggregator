package discovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeNamesFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "names.txt")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing test names file: %v", err)
	}
	return path
}

func TestLoadCompanyNames_ValidFile(t *testing.T) {
	path := writeNamesFile(t, "Acme\nGlobex\nInitech\n")

	names, err := LoadCompanyNames(path)
	if err != nil {
		t.Fatalf("LoadCompanyNames() failed: %v", err)
	}
	want := []string{"Acme", "Globex", "Initech"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("names[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestLoadCompanyNames_IgnoresBlankLinesAndComments(t *testing.T) {
	path := writeNamesFile(t, "Acme\n\n# a comment\n  \nGlobex\n")

	names, err := LoadCompanyNames(path)
	if err != nil {
		t.Fatalf("LoadCompanyNames() failed: %v", err)
	}
	if len(names) != 2 || names[0] != "Acme" || names[1] != "Globex" {
		t.Errorf("names = %v, want [Acme Globex]", names)
	}
}

func TestLoadCompanyNames_TrimsWhitespace(t *testing.T) {
	path := writeNamesFile(t, "  Acme  \n\tGlobex\t\n")

	names, err := LoadCompanyNames(path)
	if err != nil {
		t.Fatalf("LoadCompanyNames() failed: %v", err)
	}
	if len(names) != 2 || names[0] != "Acme" || names[1] != "Globex" {
		t.Errorf("names = %v, want trimmed [Acme Globex]", names)
	}
}

func TestLoadCompanyNames_DedupsCaseInsensitively(t *testing.T) {
	path := writeNamesFile(t, "Acme\nACME\nacme\nGlobex\n")

	names, err := LoadCompanyNames(path)
	if err != nil {
		t.Fatalf("LoadCompanyNames() failed: %v", err)
	}
	if len(names) != 2 || names[0] != "Acme" || names[1] != "Globex" {
		t.Errorf("names = %v, want deduped [Acme Globex] (first spelling wins)", names)
	}
}

func TestLoadCompanyNames_MissingFile(t *testing.T) {
	_, err := LoadCompanyNames("/nonexistent/names.txt")
	if err == nil {
		t.Fatal("expected an error for a nonexistent file, got nil")
	}
	if !strings.Contains(err.Error(), "opening company names file") {
		t.Errorf("error = %q, want it to mention opening the file", err.Error())
	}
}

func TestLoadCompanyNames_EmptyFileReturnsErrNoCompanyNames(t *testing.T) {
	path := writeNamesFile(t, "\n\n# just a comment\n")

	_, err := LoadCompanyNames(path)
	if err == nil {
		t.Fatal("expected an error for a file with no usable names, got nil")
	}
	if !errors.Is(err, ErrNoCompanyNames) {
		t.Errorf("error = %v, want it to match ErrNoCompanyNames", err)
	}
}
