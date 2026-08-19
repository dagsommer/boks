package secret

import (
	"bytes"
	"strings"
	"testing"
)

// These tests cover the parts of the Windows Credential Manager backend that are reachable
// without Windows: the target name and the blob encoding. The syscalls in keyring_windows.go
// are not covered by anything, here or elsewhere.
//
// The file has no build constraint on purpose — its name would earn one only if it ended in
// _windows_test.go — so that the encoding is exercised on the machines this project is
// developed on rather than on the one platform nobody here can run.

func TestCredmanTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"github", "boks:github"},
		{"anthropic-api", "boks:anthropic-api"},
		// A name that already contains a colon must not be rearranged: the prefix is
		// prepended, never parsed back out.
		{"a:b", "boks:a:b"},
		// Spaces and non-ASCII are legal secret names (validKeyringName refuses only
		// empty, over-long and control characters) and must reach the target verbatim.
		{"my token", "boks:my token"},
		{"日本", "boks:日本"},
	} {
		if got := credmanTarget(tc.name); got != tc.want {
			t.Errorf("credmanTarget(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The prefix has to be the service name the rest of the package files entries under, not a
// second spelling of it that happens to match today.
func TestCredmanTargetUsesTheServiceName(t *testing.T) {
	got := credmanTarget("x")
	if !strings.HasPrefix(got, keyringService+":") {
		t.Errorf("credmanTarget(%q) = %q, which is not under the %q service", "x", got, keyringService)
	}
}

func TestCredmanUTF16RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		what  string
		value string
	}{
		{"ascii", "hunter2"},
		{"empty", ""},
		{"japanese", "日本語のひみつ"},
		{"emoji beyond the BMP", "🔐🚀"},
		{"mixed", "tok_日本_🔐_end"},
		// A NUL is legal in a Go string and the blob is sized, not terminated, so it
		// must survive. Windows tooling that treats the blob as a C wide string will
		// show such a value truncated; Boks' own round trip is what is asserted here.
		{"embedded NUL", "before\x00after"},
		{"only a NUL", "\x00"},
		// Bytes that are valid UTF-8 but not text, to catch an encoder that assumes
		// anything about the shape of a secret.
		{"control characters", "\r\n\t\x1b[0m"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			blob, err := credmanEncodeUTF16LE(tc.value)
			if err != nil {
				t.Fatalf("encoding %q: %v", tc.what, err)
			}
			got, err := credmanDecodeUTF16LE(blob)
			if err != nil {
				t.Fatalf("decoding %q: %v", tc.what, err)
			}
			if got != tc.value {
				t.Errorf("round trip of the %s case gave %q, want %q", tc.what, got, tc.value)
			}
		})
	}
}

// The exact bytes, because a round trip alone passes just as well for UTF-8, for big-endian, or
// for any other self-consistent scheme — and what is being asserted is that the blob is the one
// the Credential Manager UI and cmdkey will read as text.
func TestCredmanEncodesLittleEndianWideChars(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []byte
	}{
		{"A", []byte{0x41, 0x00}},
		{"AB", []byte{0x41, 0x00, 0x42, 0x00}},
		// U+00E9: the low byte first is what distinguishes LE from BE, and the
		// non-zero high byte is what distinguishes UTF-16 from UTF-8.
		{"é", []byte{0xe9, 0x00}},
		// U+65E5, a plain BMP character.
		{"日", []byte{0xe5, 0x65}},
		// U+1F510 becomes the surrogate pair D83D DD10 — four bytes, not two, and
		// not the three or four a UTF-8 encoder would produce.
		{"🔐", []byte{0x3d, 0xd8, 0x10, 0xdd}},
		{"", []byte{}},
	} {
		got, err := credmanEncodeUTF16LE(tc.value)
		if err != nil {
			t.Fatalf("encoding %q: %v", tc.value, err)
		}
		if !bytes.Equal(got, tc.want) {
			t.Errorf("credmanEncodeUTF16LE(%q) = % x, want % x", tc.value, got, tc.want)
		}
	}
}

// CredentialBlobSize is set from len(blob) and Windows reads exactly that many BYTES. A
// character count in that field truncates every non-ASCII secret and is the classic bug in this
// API, so the byte counts are pinned by hand rather than derived from the encoder.
func TestCredmanBlobSizeIsBytesNotCharacters(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"hunter2", 14}, // 7 ASCII characters, 2 bytes each
		{"日本語", 6},      // 3 BMP characters
		{"🔐", 4},        // 1 character, 1 rune, 2 code units
		{"a🔐", 6},       // the mix that a rune count would get wrong
		{"", 0},
	} {
		got, err := credmanEncodeUTF16LE(tc.value)
		if err != nil {
			t.Fatalf("encoding %q: %v", tc.value, err)
		}
		if len(got) != tc.want {
			t.Errorf("credmanEncodeUTF16LE(%q) is %d bytes, want %d", tc.value, len(got), tc.want)
		}
	}
}

// A value that is not valid UTF-8 has to be refused, because utf16.Encode would otherwise
// replace what it cannot represent with U+FFFD and store a secret that is not the one it was
// given — undetectably, since the blob carries no checksum and the decode of it succeeds.
func TestCredmanEncodeRefusesInvalidUTF8(t *testing.T) {
	for _, tc := range []struct {
		what  string
		value string
	}{
		{"a lone continuation byte", "\x80"},
		{"a truncated sequence", "tok_\xe6\x97"},
		// An encoded surrogate half: valid-looking UTF-8 for a code point that
		// UTF-16 cannot represent on its own.
		{"CESU-style surrogate", "\xed\xa0\x80"},
		{"raw 0xff", "\xff\xfe"},
	} {
		blob, err := credmanEncodeUTF16LE(tc.value)
		if err == nil {
			t.Errorf("encoding %s was accepted and produced % x, want an error", tc.what, blob)
		}
	}
}

// Valid UTF-8 must not be caught by that check.
func TestCredmanEncodeAcceptsValidUTF8(t *testing.T) {
	if _, err := credmanEncodeUTF16LE("héllo 日本 🔐\x00"); err != nil {
		t.Errorf("a valid UTF-8 value was refused: %v", err)
	}
}

// An odd byte count cannot be UTF-16, so it is something another application wrote under a
// colliding target name — or a Boks that stored a different encoding. Decoding the even prefix
// would hand the caller a plausible string built from the wrong bytes.
func TestCredmanDecodeRejectsOddLength(t *testing.T) {
	for _, blob := range [][]byte{{0x41}, {0x41, 0x00, 0x42}, {0x68, 0x75, 0x6e, 0x74, 0x65}} {
		got, err := credmanDecodeUTF16LE(blob)
		if err == nil {
			t.Errorf("decoding the %d-byte blob % x returned %q, want an error", len(blob), blob, got)
		}
	}
}

func TestCredmanDecodeAcceptsEvenLength(t *testing.T) {
	got, err := credmanDecodeUTF16LE([]byte{0x41, 0x00, 0x42, 0x00})
	if err != nil {
		t.Fatalf("decoding a 4-byte blob: %v", err)
	}
	if got != "AB" {
		t.Errorf("decoded %q, want %q", got, "AB")
	}
}

// An empty blob is a legal stored value — Set writes a zero-size blob for an empty secret — and
// must decode to the empty string rather than to an error.
func TestCredmanDecodeEmptyBlob(t *testing.T) {
	got, err := credmanDecodeUTF16LE(nil)
	if err != nil {
		t.Fatalf("decoding an empty blob: %v", err)
	}
	if got != "" {
		t.Errorf("decoded %q, want the empty string", got)
	}
}
