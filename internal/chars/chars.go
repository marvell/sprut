// Package chars counts and cuts text in characters (runes), not bytes, so
// that a cut never splits a multi-byte character.
package chars

import "unicode/utf8"

// Cut is s cut to its first n characters, and how many characters it drops.
func Cut(s string, n int) (kept string, dropped int) {
	for i := range s { // i is the byte offset of each character
		if n == 0 {
			return s[:i], utf8.RuneCountInString(s[i:])
		}
		n--
	}
	return s, 0
}
