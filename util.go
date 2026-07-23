package main

import (
	"crypto/sha1"
	"encoding/hex"
	"path"
	"strconv"
)

// pathKey returns a stable, filesystem-safe key for a relative archive path.
// It is used to name the archive's cached cover file, so a cover can always be
// re-derived from the path alone (nothing about the cache is authoritative).
func pathKey(rel string) string {
	sum := sha1.Sum([]byte(rel))
	return hex.EncodeToString(sum[:])
}

// identityOf returns the durable identity of an archive from its relative path.
//
// If the filename begins with an integer followed by a space (e.g. "0042 Foo.zip"
// or "-7 Foo.zip"), that integer is the identity — returned as "#42" / "#-7".
// A leading "-" is allowed so hand-assigned IDs can live in the negatives and
// never collide with a source that hands out positive IDs. Parsing as an integer
// means padding is irrelevant ("007" and "7" match). This identity survives
// moves, renames of the descriptive part, and even re-encoding, because it lives
// in the name, not the bytes.
//
// Files that don't follow the convention fall back to "@"+path, which is stable
// only until the file is moved or renamed. The "#"/"@" prefixes keep the two
// kinds from ever colliding and make the identity easy to read.
func identityOf(rel string) string {
	base := path.Base(rel)
	i := 0
	if i < len(base) && base[i] == '-' {
		i++ // optional leading sign
	}
	start := i
	for i < len(base) && base[i] >= '0' && base[i] <= '9' {
		i++
	}
	if i > start && i < len(base) && base[i] == ' ' {
		if n, err := strconv.ParseInt(base[:i], 10, 64); err == nil {
			return "#" + strconv.FormatInt(n, 10)
		}
	}
	return "@" + rel
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
