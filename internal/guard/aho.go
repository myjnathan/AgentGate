// Package guard implements the gateway protection pipeline's detection
// primitives: an Aho-Corasick multi-pattern matcher for word lists and
// regex/NER-style detectors for structured sensitive data.
package guard

import "strings"

// AhoCorasick matches many patterns in one pass over the text.
type AhoCorasick struct {
	next  []map[rune]int
	fail  []int
	out   [][]string
	empty bool
}

func NewAhoCorasick(patterns []string) *AhoCorasick {
	ac := &AhoCorasick{next: []map[rune]int{{}}, fail: []int{0}, out: [][]string{{}}}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		node := 0
		for _, r := range p {
			if _, ok := ac.next[node][r]; !ok {
				ac.next[node][r] = len(ac.next)
				ac.next = append(ac.next, map[rune]int{})
				ac.fail = append(ac.fail, 0)
				ac.out = append(ac.out, nil)
			}
			node = ac.next[node][r]
		}
		ac.out[node] = append(ac.out[node], p)
	}
	ac.empty = len(ac.next) == 1
	// Build failure links (BFS).
	queue := []int{}
	for _, v := range ac.next[0] {
		ac.fail[v] = 0
		queue = append(queue, v)
	}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for r, v := range ac.next[u] {
			f := ac.fail[u]
			for f != 0 {
				if _, ok := ac.next[f][r]; ok {
					break
				}
				f = ac.fail[f]
			}
			if nxt, ok := ac.next[f][r]; ok && nxt != v {
				ac.fail[v] = nxt
			} else {
				ac.fail[v] = 0
			}
			ac.out[v] = append(ac.out[v], ac.out[ac.fail[v]]...)
			queue = append(queue, v)
		}
	}
	return ac
}

// Find returns the distinct patterns found in text.
func (ac *AhoCorasick) Find(text string) []string {
	if ac.empty {
		return nil
	}
	found := map[string]bool{}
	var res []string
	node := 0
	for _, r := range text {
		for node != 0 {
			if _, ok := ac.next[node][r]; ok {
				break
			}
			node = ac.fail[node]
		}
		if nxt, ok := ac.next[node][r]; ok {
			node = nxt
		}
		for _, p := range ac.out[node] {
			if !found[p] {
				found[p] = true
				res = append(res, p)
			}
		}
	}
	return res
}
