package main

import (
	"regexp"
	"strconv"
	"strings"
)

// Volume grouping: archives whose names differ only in their volume number
// ("Foo Vol. 1", "Foo Vol. 2", "Foo v3") belong to one series and are shown as
// a single card in the library.

var (
	leadingID = regexp.MustCompile(`^-?\d+\s+`)
	// A volume marker right before the number: "Vol.", "Volume", "v", "#", ...
	volMarker = regexp.MustCompile(`(?i)(?:\b(?:vol|volume|v|no|part|pt|book)\.?|#)\s*$`)
	spaces    = regexp.MustCompile(`\s+`)
)

// seriesOf splits an archive title into its series and volume number.
//
// The volume number is a suffix: "Some Series Name 01", "Foo Vol. 1". The
// leading "#id " (see identityOf) is ignored, since every volume has its own ID,
// so a number leading the name is never the volume. The volume is the last
// number in the remaining name that isn't inside (), [] or {} — those usually
// hold release notes like "[1080p]" or "(2019)". A volume marker right before
// the number is dropped so "Foo Vol. 1" and "Foo v2" are the same series;
// whatever follows the number must match too.
//
// key is the normalized series name used for grouping (lowercase, collapsed
// whitespace, no double quotes so it can be written in a search query); name is
// a display form. ok is false when the title has no volume number, in which
// case the archive never groups with anything.
func seriesOf(title string) (key, name string, vol int, ok bool) {
	s := leadingID.ReplaceAllString(strings.TrimSpace(title), "")
	if pre, n, found := trailingVolume(s); found {
		return finishSeries(pre, n)
	}
	return "", "", 0, false
}

// trailingVolume finds the last number in s outside brackets that has a name in
// front of it, and returns the name around it (marker dropped) and the number.
func trailingVolume(s string) (name string, vol int, ok bool) {
	start, end, depth := -1, -1, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			if depth > 0 {
				depth--
			}
		case isDigit(c) && depth == 0:
			j := runEnd(s, i)
			start, end = i, j
			i = j - 1
		}
	}
	if start < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(s[start:end])
	if err != nil {
		return "", 0, false
	}
	pre := volMarker.ReplaceAllString(s[:start], "")
	pre = strings.TrimSpace(strings.TrimRight(pre, " -_.,:;~"))
	if pre == "" {
		return "", 0, false // nothing but a number
	}
	if suf := strings.TrimSpace(s[end:]); suf != "" {
		pre += " " + suf
	}
	return pre, n, true
}

func finishSeries(name string, vol int) (string, string, int, bool) {
	name = spaces.ReplaceAllString(strings.TrimSpace(name), " ")
	key := strings.TrimSpace(strings.ToLower(strings.ReplaceAll(name, `"`, "")))
	if key == "" {
		return "", "", 0, false
	}
	return key, name, vol, true
}
