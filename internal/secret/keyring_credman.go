package secret

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// The parts of the Windows Credential Manager backend that are not syscalls.
//
// This file has no build constraint, and that is the whole point of it existing separately from
// keyring_windows.go. Nothing in that file has ever been executed, because nobody working on
// this project has a Windows machine in the loop; these three functions have, by
// keyring_windows_helpers_test.go, on every machine that runs `go test ./internal/secret/`. The
// split is what decides whether the encoding — the part that silently corrupts values rather
// than failing — is tested or merely written.
//
// They are unused on every platform but Windows. That is deliberate and is the price of being
// able to test them at all.

// credmanTarget is the TargetName one Boks secret is filed under in the Credential Manager.
//
// The prefix does two jobs. It makes the entries identifiable in the Credential Manager UI and
// to `cmdkey /list`, where they otherwise appear among the dozens Windows and Office put there
// and a user has no way to tell what created one. And it namespaces them: TargetName is a flat
// global string per credential type, shared by every application on the machine, so a secret
// named "github" without a prefix would collide with whatever else on the system decided that
// "github" was a reasonable key — one program's write silently replacing another's.
//
// A colon rather than a slash or a backslash: Windows itself uses "LegacyGeneric:target=..."
// and "MicrosoftAccount:user=..." in this field, so a colon is the convention the UI groups on,
// while a backslash reads as a domain separator to anything parsing the older forms.
func credmanTarget(name string) string {
	return keyringService + ":" + name
}

// credmanEncodeUTF16LE encodes a secret for the CredentialBlob.
//
// # Why UTF-16LE
//
// The blob is uninterpreted bytes with a byte count, so any encoding round trips as far as
// Windows is concerned, and the choice is entirely about who else reads it. Everything on the
// platform that shows a generic credential's blob as text — the Credential Manager UI,
// `cmdkey`, PowerShell's credential types, the wincred samples — reads it as a wide string,
// because that is what a Windows API means by text. Storing UTF-8 would mean a value that
// looks right in Boks and like mojibake everywhere else the user might look at it, which is a
// bad thing to be right about alone.
//
// # What it costs
//
// Two things, both worth stating rather than discovering.
//
// The blob limit is 2560 bytes, so UTF-16LE halves the longest storable secret to 1280
// characters where UTF-8 would have allowed 2560 ASCII ones. That is far above any API token
// and far below a private key file, which is not a thing this backend should be holding.
//
// And the encoding is not recorded anywhere in the credential. A value written by some future
// or past Boks in another encoding is not detected here — it is decoded as UTF-16LE and comes
// back as unreadable text, not as an error, except in the one case where the byte count is odd.
// If this ever needs to change, the change has to be a new TargetName prefix, not a new
// encoding under the old one.
//
// Invalid UTF-8 is refused rather than mangled. utf16.Encode maps anything it cannot represent
// to U+FFFD, so a value that is not valid UTF-8 would be stored lossily and read back as a
// different secret — silently, and only noticed when whatever the secret authenticates to
// starts rejecting it. Refusing means every value this function accepts survives the round trip
// exactly, including NULs, non-BMP runes and any other valid text.
func credmanEncodeUTF16LE(value string) ([]byte, error) {
	if !utf8.ValidString(value) {
		return nil, errors.New("the value is not valid UTF-8, so it cannot be stored in the Credential Manager without being altered")
	}
	units := utf16.Encode([]rune(value))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[2*i:], u)
	}
	return b, nil
}

// credmanDecodeUTF16LE reverses credmanEncodeUTF16LE.
//
// The odd-length case is the only cross-check available. A blob that is not a whole number of
// UTF-16 code units cannot be something this code wrote, so it is far more likely to be another
// application's credential under a colliding target name, or a Boks that stored UTF-8, than
// anything worth decoding — and half-decoding it would hand the caller a plausible-looking
// string built from the wrong bytes. Everything else has to be taken on trust: UTF-16 has no
// other shape to check.
//
// utf16.Decode substitutes U+FFFD for an unpaired surrogate, which cannot occur in anything the
// encoder produced (it rejects the inputs that would cause one) and can occur in a blob written
// by something else. So a wrong-encoding read degrades to visible mojibake, never to an error.
func credmanDecodeUTF16LE(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", fmt.Errorf("the stored value is %d bytes, which is not a whole number of UTF-16 code units", len(b))
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units)), nil
}
