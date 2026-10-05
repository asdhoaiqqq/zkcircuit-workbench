package zkcircuit

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

// A circuit name is part of the version's identity: a circuit created under
// a name must be retrievable under that same name forever. encoding/json
// silently rewrites strings that are not complete, legal UTF-8 — invalid
// bytes become U+FFFD on both Marshal and Unmarshal, and so does a \uXXXX
// escape naming an unpaired surrogate — so a name accepted with such content
// would be committed, and read back, as a different string than the one
// submitted: the just-created circuit would have changed names. The two
// checks below close both ends of that hole:
//
//   - validateCircuitNameUTF8 refuses invalid UTF-8 at the creation entry,
//     so a bad name is never persisted in a rewritten form;
//   - checkStoredNameRaw inspects the raw JSON string literal of a committed
//     name and refuses the read when the stored text could not survive a
//     lossless decode, instead of carrying on with the replaced value.
//
// Only the circuit name is held to this rule; every other field keeps its
// existing reading rules. A name that genuinely is the replacement character
// "�" (U+FFFD, legal UTF-8) is an ordinary name, a legal surrogate pair
// reads as the character it encodes, and a name that is literally the
// six-character text \uD800 (written in the file with an escaped backslash)
// stays exactly that literal text. Names keep being compared byte-exactly:
// no trimming, no case folding, no normalization.

// validateCircuitNameUTF8 enforces the creation-time half of the name rule:
// beyond being non-empty and non-blank, a submitted name must be complete,
// legal UTF-8 — no lone continuation bytes, no truncated multi-byte
// characters, no overlong encodings, no surrogate code points written as
// UTF-8. The check runs before any state is touched, so a rejected name
// fails the whole create request with ErrInvalidArgument and nothing is
// staged or committed.
func validateCircuitNameUTF8(name string) error {
	if !utf8.ValidString(name) {
		return invalidf("circuit name %q is not valid UTF-8: a name must be complete, legal UTF-8 so it can be committed and read back unchanged", name)
	}
	return nil
}

// checkStoredNameRaw enforces the read-time half of the name rule on the raw
// JSON string literal of a committed circuit record's name field. The caller
// has already proven raw is a syntactically valid JSON string, so escape
// sequences are well-formed; what remains invisible to the ordinary decode
// is exactly what must be rejected here:
//
//   - bytes outside escapes that are not legal UTF-8 (a lone continuation
//     byte, a truncated multi-byte character, an overlong form, a surrogate
//     code point encoded as UTF-8) — encoding/json would decode them as
//     U+FFFD, renaming the circuit on read;
//   - a \uXXXX escape naming an unpaired surrogate — a high surrogate not
//     immediately followed by a low-surrogate escape, or a low surrogate
//     standing alone — which encoding/json would likewise replace with
//     U+FFFD.
//
// Both are data corruption: the read is refused and names the record's name
// field rather than continuing with the substituted character. Legal content
// is untouched: a real "�" byte sequence, a paired emoji escape, and the
// literal text \uD800 behind an escaped backslash all keep their exact
// values.
func checkStoredNameRaw(raw json.RawMessage, what string) error {
	trimmed := bytes.TrimSpace(raw)
	// raw is a complete, syntactically valid JSON string literal (the caller
	// decoded it already), so it is at least two quotes and every escape is
	// well-formed; the scan below relies on that for its bounds.
	body := trimmed[1 : len(trimmed)-1]
	for i := 0; i < len(body); {
		b := body[i]
		switch {
		case b == '\\':
			if body[i+1] != 'u' {
				i += 2 // any other escape is two bytes and always legal here
				continue
			}
			cp := hex4Value(body[i+2 : i+6])
			switch {
			case cp >= 0xD800 && cp <= 0xDBFF:
				// A high surrogate is whole only when a low-surrogate escape
				// follows it immediately; anything else — end of string, a
				// plain character, a non-surrogate escape, a second high
				// surrogate — leaves it unpaired.
				if i+12 <= len(body) && body[i+6] == '\\' && body[i+7] == 'u' {
					if lo := hex4Value(body[i+8 : i+12]); lo >= 0xDC00 && lo <= 0xDFFF {
						i += 12
						continue
					}
				}
				return corruptf("%s carries an unpaired high-surrogate escape \\u%04X: the committed name cannot be read back unchanged", what, cp)
			case cp >= 0xDC00 && cp <= 0xDFFF:
				return corruptf("%s carries an unpaired low-surrogate escape \\u%04X: the committed name cannot be read back unchanged", what, cp)
			default:
				i += 6
			}
		case b < 0x80:
			i++
		default:
			// A multi-byte character must be a legal UTF-8 encoding. U+FFFD
			// itself decodes with size 3 and stays legal; size 1 is the
			// decoder's "invalid byte" report.
			r, size := utf8.DecodeRune(body[i:])
			if r == utf8.RuneError && size == 1 {
				return corruptf("%s contains bytes that are not valid UTF-8: the committed name cannot be read back unchanged", what)
			}
			i += size
		}
	}
	return nil
}

// hex4Value parses exactly four hexadecimal digits (already validated as hex
// by the JSON grammar check) into their code-point value.
func hex4Value(d []byte) uint16 {
	var v uint16
	for _, c := range d {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint16(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint16(c-'A') + 10
		}
	}
	return v
}
