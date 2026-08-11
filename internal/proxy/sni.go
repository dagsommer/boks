package proxy

import (
	"errors"
	"fmt"
)

// errNoClientHello reports that the bytes are not a TLS handshake at all.
var errNoClientHello = errors.New("not a TLS ClientHello")

// errNoSNI reports a well-formed ClientHello that carries no server_name extension.
var errNoSNI = errors.New("ClientHello contains no server name (SNI)")

// maxClientHello bounds how much a client may send before the proxy gives up looking for a
// ClientHello. A TLS record cannot exceed 16 KiB of payload, and a ClientHello that needs
// more than one record is not something Boks will guess at.
const maxClientHello = 16 * 1024

// extractSNI parses the server name out of a TLS ClientHello record.
//
// crypto/tls has no exported way to do this — it parses a handshake it is going to
// complete — and Boks must read the name *without* completing anything, because completing
// it would mean terminating TLS. So this is a deliberately narrow, allocation-free walk
// over the record with a bounds check at every step. It never interprets anything beyond
// the server_name extension and never keeps the bytes it walked over.
//
// The input is the raw bytes read from the client, starting at the record header.
// It returns errShortClientHello (via ok=false) if more bytes are needed.
func extractSNI(buf []byte) (name string, needMore bool, err error) {
	r := reader(buf)

	// TLSPlaintext: type(1) version(2) length(2)
	recType, ok := r.u8()
	if !ok {
		return "", true, nil
	}
	if recType != 0x16 { // handshake
		return "", false, errNoClientHello
	}
	if _, ok := r.skip(2); !ok { // legacy record version, meaningless in TLS 1.3
		return "", true, nil
	}
	recLen, ok := r.u16()
	if !ok {
		return "", true, nil
	}
	if recLen == 0 || int(recLen) > maxClientHello {
		return "", false, fmt.Errorf("%w: implausible record length %d", errNoClientHello, recLen)
	}
	body, ok := r.take(int(recLen))
	if !ok {
		return "", true, nil
	}
	h := reader(body)

	// Handshake: msg_type(1) length(3)
	msgType, ok := h.u8()
	if !ok || msgType != 0x01 { // client_hello
		return "", false, errNoClientHello
	}
	if _, ok := h.skip(3); !ok {
		return "", false, errNoClientHello
	}
	// ClientHello: legacy_version(2) random(32)
	if _, ok := h.skip(2 + 32); !ok {
		return "", false, errNoClientHello
	}
	if _, ok := h.vector(1); !ok { // legacy_session_id
		return "", false, errNoClientHello
	}
	if _, ok := h.vector(2); !ok { // cipher_suites
		return "", false, errNoClientHello
	}
	if _, ok := h.vector(1); !ok { // legacy_compression_methods
		return "", false, errNoClientHello
	}
	exts, ok := h.vector(2)
	if !ok {
		return "", false, errNoSNI
	}

	e := reader(exts)
	for len(e) > 0 {
		extType, ok := e.u16()
		if !ok {
			return "", false, errNoSNI
		}
		extData, ok := e.vector(2)
		if !ok {
			return "", false, errNoSNI
		}
		if extType != 0x0000 { // server_name
			continue
		}
		// ServerNameList: list(2) [ name_type(1) HostName(2) ]*
		sn := reader(extData)
		list, ok := sn.vector(2)
		if !ok {
			return "", false, errNoSNI
		}
		l := reader(list)
		for len(l) > 0 {
			nameType, ok := l.u8()
			if !ok {
				return "", false, errNoSNI
			}
			host, ok := l.vector(2)
			if !ok {
				return "", false, errNoSNI
			}
			if nameType == 0 { // host_name
				if len(host) == 0 {
					return "", false, errNoSNI
				}
				return string(host), false, nil
			}
		}
		return "", false, errNoSNI
	}
	return "", false, errNoSNI
}

// reader is a cursor over a byte slice with checked reads. Every method reports failure
// rather than panicking, because the bytes come from a hostile guest.
type reader []byte

func (r *reader) u8() (byte, bool) {
	if len(*r) < 1 {
		return 0, false
	}
	b := (*r)[0]
	*r = (*r)[1:]
	return b, true
}

func (r *reader) u16() (uint16, bool) {
	if len(*r) < 2 {
		return 0, false
	}
	v := uint16((*r)[0])<<8 | uint16((*r)[1])
	*r = (*r)[2:]
	return v, true
}

func (r *reader) skip(n int) ([]byte, bool) { return r.take(n) }

func (r *reader) take(n int) ([]byte, bool) {
	if n < 0 || len(*r) < n {
		return nil, false
	}
	b := (*r)[:n]
	*r = (*r)[n:]
	return b, true
}

// vector reads a length-prefixed vector whose length occupies lenBytes bytes.
func (r *reader) vector(lenBytes int) ([]byte, bool) {
	var n int
	for i := 0; i < lenBytes; i++ {
		b, ok := r.u8()
		if !ok {
			return nil, false
		}
		n = n<<8 | int(b)
	}
	return r.take(n)
}
