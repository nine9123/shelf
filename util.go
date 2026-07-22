package main

import (
	"crypto/sha1"
	"encoding/hex"
)

// pathKey returns a stable, filesystem-safe key for a relative archive path.
// It is used to name the archive's cached cover file, so a cover can always be
// re-derived from the path alone (nothing about the cache is authoritative).
func pathKey(rel string) string {
	sum := sha1.Sum([]byte(rel))
	return hex.EncodeToString(sum[:])
}

// naturalLess compares two strings so that embedded numbers sort by value:
// "page2.jpg" < "page10.jpg". This is what puts pages in reading order even
// when filenames are not zero-padded.
func naturalLess(a, b string) bool {
	ia, ib := 0, 0
	for ia < len(a) && ib < len(b) {
		ca, cb := a[ia], b[ib]
		if isDigit(ca) && isDigit(cb) {
			ja, jb := runEnd(a, ia), runEnd(b, ib)
			na, nb := trimZeros(a[ia:ja]), trimZeros(b[ib:jb])
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			ia, ib = ja, jb
			continue
		}
		la, lb := toLower(ca), toLower(cb)
		if la != lb {
			return la < lb
		}
		ia++
		ib++
	}
	return len(a)-ia < len(b)-ib
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func toLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

func runEnd(s string, i int) int {
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i
}

func trimZeros(s string) string {
	k := 0
	for k < len(s)-1 && s[k] == '0' {
		k++
	}
	return s[k:]
}
