package main

import "strings"

type tagPair struct {
	ns  string
	val string
}

// parseInfo extracts the category and tag pairs from an info.txt payload.
// Recognized lines:
//
//	Category: <name>
//	> <namespace>: <v1>, <v2>, ...
//
// Length is intentionally ignored — the archive's image count is the length.
func parseInfo(meta string) (category string, tags []tagPair) {
	for _, line := range strings.Split(meta, "\n") {
		t := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, ">") {
			rest := strings.TrimSpace(t[1:])
			i := strings.IndexByte(rest, ':')
			if i <= 0 {
				continue
			}
			ns := strings.TrimSpace(rest[:i])
			if ns == "" {
				continue
			}
			for _, v := range strings.Split(rest[i+1:], ",") {
				if v = strings.TrimSpace(v); v != "" {
					tags = append(tags, tagPair{ns, v})
				}
			}
			continue
		}
		if len(t) >= 9 && strings.EqualFold(t[:9], "category:") {
			category = strings.TrimSpace(t[9:])
		}
	}
	return
}

// tokenizeQuery splits a search string on spaces, honoring double quotes so
// multi-word values work: artist:"john doe" becomes one token `artist:john doe`.
func tokenizeQuery(q string) []string {
	var toks []string
	var b strings.Builder
	inQuote := false
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return toks
}

// parseQuery classifies tokens into free-text terms, namespaced tag filters, and
// an optional category. A token "ns:value" is a tag filter (or category filter
// when ns == "category"); anything else is a free-text term matched on the
// title, path, and raw info text. All are combined with AND by the caller.
func parseQuery(q string) (terms []string, tags []tagPair, category string) {
	for _, tok := range tokenizeQuery(q) {
		i := strings.IndexByte(tok, ':')
		if i > 0 && i < len(tok)-1 {
			ns, val := tok[:i], tok[i+1:]
			if strings.EqualFold(ns, "category") {
				category = val
			} else {
				tags = append(tags, tagPair{ns, val})
			}
			continue
		}
		terms = append(terms, tok)
	}
	return
}
