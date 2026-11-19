// Package fuzzy implements fzf-style filtering: space-separated terms that
// must all match (AND), each a fuzzy subsequence match by default, with
// fzf's extended syntax:
//
//	term    fuzzy match            'term   exact substring
//	^term   prefix                 term$   suffix
//	!term   must not match (exact substring, like fzf)
//
// Smart case: a term containing an upper-case letter is case-sensitive.
// Each term must match within a single field (column), so characters
// scattered over unrelated columns do not count as a match.
package fuzzy

import (
	"strings"
	"unicode"
)

type kind int

const (
	kFuzzy kind = iota
	kExact
	kPrefix
	kSuffix
)

type term struct {
	text      string
	kind      kind
	negate    bool
	sensitive bool
}

// Pattern is a parsed query.
type Pattern struct{ terms []term }

func Parse(query string) Pattern {
	var p Pattern
	for f := range strings.FieldsSeq(query) {
		t := term{kind: kFuzzy}
		if strings.HasPrefix(f, "!") {
			t.negate, t.kind = true, kExact
			f = f[1:]
		}
		switch {
		case strings.HasPrefix(f, "'"):
			t.kind, f = kExact, f[1:]
		case strings.HasPrefix(f, "^"):
			t.kind, f = kPrefix, f[1:]
		case strings.HasSuffix(f, "$") && len(f) > 1:
			t.kind, f = kSuffix, strings.TrimSuffix(f, "$")
		}
		if f == "" {
			continue
		}
		t.sensitive = strings.IndexFunc(f, unicode.IsUpper) >= 0
		if !t.sensitive {
			f = strings.ToLower(f)
		}
		t.text = f
		p.terms = append(p.terms, t)
	}
	return p
}

// Empty reports whether the pattern matches everything.
func (p Pattern) Empty() bool { return len(p.terms) == 0 }

// Match returns whether all terms match one of the fields, and a score
// (higher is better).
func (p Pattern) Match(fields []string) (int, bool) {
	lower := make([]string, len(fields))
	for i, f := range fields {
		lower[i] = strings.ToLower(f)
	}
	total := 0
	for _, t := range p.terms {
		best, ok := -1, false
		for i := range fields {
			hay := lower[i]
			if t.sensitive {
				hay = fields[i]
			}
			if s, m := t.match(hay); m && s > best {
				best, ok = s, true
			}
		}
		if t.negate {
			if ok {
				return 0, false
			}
			continue
		}
		if !ok {
			return 0, false
		}
		total += best
	}
	return total, true
}

const (
	scoreMatch       = 16
	bonusConsecutive = 8
	bonusBoundary    = 10
	bonusFirstChar   = 4
	penaltyGapStart  = 3
	penaltyGapExt    = 1
)

func (t term) match(hay string) (int, bool) {
	switch t.kind {
	case kExact:
		i := strings.Index(hay, t.text)
		if i < 0 {
			return 0, false
		}
		return exactScore(hay, i, len(t.text)), true
	case kPrefix:
		if !strings.HasPrefix(hay, t.text) {
			return 0, false
		}
		return exactScore(hay, 0, len(t.text)), true
	case kSuffix:
		if !strings.HasSuffix(hay, t.text) {
			return 0, false
		}
		return exactScore(hay, len(hay)-len(t.text), len(t.text)), true
	}
	return fuzzyScore([]rune(hay), []rune(t.text))
}

func exactScore(hay string, start, n int) int {
	s := n*(scoreMatch+bonusConsecutive) - bonusConsecutive
	if isBoundary([]rune(hay), len([]rune(hay[:start]))) {
		s += bonusBoundary
	}
	return s
}

// fuzzyScore finds the needle as a subsequence: forward scan to the end of
// the first complete match, backward scan for the tightest window (fzf v1),
// then scores consecutive runs, word boundaries and gaps.
func fuzzyScore(hay, needle []rune) (int, bool) {
	if len(needle) == 0 {
		return 0, true
	}
	ni, end := 0, -1
	for i, r := range hay {
		if r == needle[ni] {
			ni++
			if ni == len(needle) {
				end = i
				break
			}
		}
	}
	if end < 0 {
		return 0, false
	}
	ni, start := len(needle)-1, end
	for i := end; i >= 0; i-- {
		if hay[i] == needle[ni] {
			ni--
			if ni < 0 {
				start = i
				break
			}
		}
	}
	score, ni, prev, inGap := 0, 0, -2, false
	for i := start; i <= end && ni < len(needle); i++ {
		if hay[i] != needle[ni] {
			if !inGap {
				score -= penaltyGapStart
				inGap = true
			} else {
				score -= penaltyGapExt
			}
			continue
		}
		inGap = false
		score += scoreMatch
		if prev == i-1 {
			score += bonusConsecutive
		}
		if isBoundary(hay, i) {
			score += bonusBoundary
			if ni == 0 {
				score += bonusFirstChar
			}
		}
		prev = i
		ni++
	}
	return score, true
}

func isBoundary(hay []rune, i int) bool {
	if i == 0 {
		return true
	}
	switch hay[i-1] {
	case '-', '_', '/', '.', ':', ' ':
		return true
	}
	return false
}
