package fuzzy

import (
	"sort"
	"testing"
)

func TestMatch(t *testing.T) {
	rows := [][]string{
		{"immich/valkey-data", "healthy", "pvc-911615be"},
		{"paperless-ngx/valkey-data", "healthy", "pvc-8be52e2f"},
		{"stash/stash-sense-data", "healthy", "pvc-966ca5a6"},
		{"mc/data", "degraded", "pvc-e81c8b86"},
	}
	match := func(q string) []string {
		p := Parse(q)
		type r struct {
			name  string
			score int
		}
		var out []r
		for _, row := range rows {
			if s, ok := p.Match(row); ok {
				out = append(out, r{row[0], s})
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
		names := make([]string, len(out))
		for i := range out {
			names[i] = out[i].name
		}
		return names
	}
	cases := []struct {
		q    string
		want []string
	}{
		{"valk data", []string{"immich/valkey-data", "paperless-ngx/valkey-data"}},
		{"immich valk", []string{"immich/valkey-data"}},
		{"valk !paper", []string{"immich/valkey-data"}},
		{"^mc", []string{"mc/data"}},
		{"degr", []string{"mc/data"}},
		{"'sense", []string{"stash/stash-sense-data"}},
		{"data$ !valk !sense", []string{"mc/data"}},
		{"Valk", nil}, // smart case: upper-case term is case-sensitive
	}
	for _, c := range cases {
		got := match(c.q)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v, want %v", c.q, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: got %v, want %v", c.q, got, c.want)
				break
			}
		}
	}
}

func TestScorePrefersTightMatches(t *testing.T) {
	p := Parse("data")
	a, _ := p.Match([]string{"mc/data"})
	b, _ := p.Match([]string{"d-a-t-a"})
	if a <= b {
		t.Errorf("contiguous match should score higher: %d vs %d", a, b)
	}
	// fields are matched separately: no match across columns
	if _, ok := Parse("hdat").Match([]string{"healthy", "data"}); ok {
		t.Error("terms must match within one field")
	}
}
