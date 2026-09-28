package relevance

import "testing"

func TestRelevantVocabulary_IncludesTheExampleKeywordsAndOnlyRelevantFamilies(t *testing.T) {
	vocab := RelevantVocabulary()
	if len(vocab) == 0 {
		t.Fatal("RelevantVocabulary: got no phrases")
	}
	want := map[string]bool{"golang": false, "backend": false, "data engineer": false, "devops": false}
	seen := make(map[string]bool)
	for _, phrase := range vocab {
		if seen[phrase] {
			t.Errorf("RelevantVocabulary: duplicate phrase %q", phrase)
		}
		seen[phrase] = true
		if _, ok := want[phrase]; ok {
			want[phrase] = true
		}
	}
	for phrase, found := range want {
		if !found {
			t.Errorf("RelevantVocabulary: missing expected phrase %q", phrase)
		}
	}
	// A phrase belonging only to a non-relevant family (other_tech) must not appear.
	for _, phrase := range vocab {
		if phrase == "solutions architect" {
			t.Errorf("RelevantVocabulary: %q belongs to other_tech, not a relevant family, but was included", phrase)
		}
	}
	for i := 1; i < len(vocab); i++ {
		if vocab[i-1] >= vocab[i] {
			t.Fatalf("RelevantVocabulary: not sorted at index %d: %q >= %q", i, vocab[i-1], vocab[i])
		}
	}
}

func TestContainsPhrase_WholeWordOnly(t *testing.T) {
	cases := []struct {
		title, phrase string
		want          bool
	}{
		{"Senior Backend Engineer (Golang)", "golang", true},
		{"Senior Backend Engineer (Golang)", "backend", true},
		{"Frontend Developer", "java", false},   // must not match inside "javascript" if title had it
		{"JavaScript Developer", "java", false}, // whole-word: "java" is not a word in "javascript"
		{"Java Backend Engineer", "java", true},
		{"Data Engineer", "data engineer", true},
		{"Metadata Engineer", "data engineer", false},
	}
	for _, c := range cases {
		got := ContainsPhrase(Words(c.title), c.phrase)
		if got != c.want {
			t.Errorf("ContainsPhrase(Words(%q), %q) = %v, want %v", c.title, c.phrase, got, c.want)
		}
	}
}
