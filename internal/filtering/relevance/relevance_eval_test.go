package relevance

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// The eval: real job titles (the dev set of ../testdata/dev.jsonl) with the
// independent labeler's role family. The bars are the frozen rubric's:
// relevance precision and recall at least 0.90, role family accuracy at least
// 0.80.

type devRow struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Gold  struct {
		RoleFamily string `json:"role_family"`
		Relevant   bool   `json:"relevant"`
	} `json:"gold"`
}

func loadDev(t *testing.T) []devRow {
	t.Helper()
	f, err := os.Open("../testdata/dev.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []devRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r devRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestDevGold_RoleFamilyAndRelevance(t *testing.T) {
	p := Default()
	rows := loadDev(t)
	var famOK, tp, fp, fn int
	var misses []string
	for _, r := range rows {
		got := p.Classify(r.Title)
		if got.Family == r.Gold.RoleFamily {
			famOK++
		} else {
			misses = append(misses, r.Gold.RoleFamily+" <- "+got.Family+" ["+got.Matched+"]\t"+r.Title)
		}
		switch {
		case got.Relevant && r.Gold.Relevant:
			tp++
		case got.Relevant && !r.Gold.Relevant:
			fp++
		case !got.Relevant && r.Gold.Relevant:
			fn++
		}
	}
	precision := float64(tp) / float64(tp+fp)
	recall := float64(tp) / float64(tp+fn)
	acc := float64(famOK) / float64(len(rows))
	t.Logf("dev gold: %d titles | relevance precision %.3f recall %.3f | role family accuracy %.3f", len(rows), precision, recall, acc)
	if path := os.Getenv("PHASE4_DUMP_ROLES"); path != "" {
		sort.Strings(misses)
		_ = os.WriteFile(path, []byte(strings.Join(misses, "\n")+"\n"), 0o644)
	}
	if precision < 0.90 {
		t.Errorf("relevance precision %.3f, want >= 0.90", precision)
	}
	if recall < 0.90 {
		t.Errorf("relevance recall %.3f, want >= 0.90", recall)
	}
	if acc < 0.80 {
		t.Errorf("role family accuracy %.3f, want >= 0.80", acc)
	}
}
