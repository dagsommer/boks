package proxy

import (
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

// captureClientHello performs the client half of a TLS handshake against a pipe and
// returns the raw bytes of the ClientHello, so the parser is tested against records that
// crypto/tls actually produced rather than ones this test invented.
func captureClientHello(t *testing.T, cfg *tls.Config) []byte {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 8192)
		server.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _ := server.Read(buf)
		done <- buf[:n]
		server.Close()
	}()
	c := tls.Client(client, cfg)
	client.SetDeadline(time.Now().Add(5 * time.Second))
	_ = c.Handshake() // fails once the peer closes; we only want the first flight
	client.Close()
	return <-done
}

func TestExtractSNIFromRealClientHello(t *testing.T) {
	hello := captureClientHello(t, &tls.Config{ServerName: "example.com"})
	name, needMore, err := extractSNI(hello)
	if err != nil {
		t.Fatalf("extractSNI: %v", err)
	}
	if needMore {
		t.Fatal("extractSNI wanted more bytes from a complete ClientHello")
	}
	if name != "example.com" {
		t.Errorf("SNI = %q, want example.com", name)
	}
}

func TestExtractSNIWithoutServerName(t *testing.T) {
	// An IP-literal target produces a ClientHello with no server_name extension. This is
	// the documented blind spot: the CONNECT target is the only name available.
	hello := captureClientHello(t, &tls.Config{ServerName: "127.0.0.1"})
	_, needMore, err := extractSNI(hello)
	if needMore {
		t.Fatal("extractSNI wanted more bytes")
	}
	if !errors.Is(err, errNoSNI) {
		t.Errorf("err = %v, want errNoSNI", err)
	}
}

func TestExtractSNIWantsMoreBytes(t *testing.T) {
	hello := captureClientHello(t, &tls.Config{ServerName: "example.com"})
	for _, n := range []int{1, 2, 5, 20, len(hello) - 1} {
		if n < 0 || n >= len(hello) {
			continue
		}
		name, needMore, err := extractSNI(hello[:n])
		if err != nil {
			t.Errorf("extractSNI(first %d bytes) = error %v, want a request for more", n, err)
		}
		if !needMore {
			t.Errorf("extractSNI(first %d bytes) returned %q without asking for more", n, name)
		}
	}
}

func TestExtractSNIRejectsNonTLS(t *testing.T) {
	_, needMore, err := extractSNI([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	if needMore {
		t.Fatal("plain HTTP was treated as an incomplete ClientHello")
	}
	if !errors.Is(err, errNoClientHello) {
		t.Errorf("err = %v, want errNoClientHello", err)
	}
}

// TestExtractSNIOnHostileInput feeds the parser truncated, oversized and structurally
// lying records. It must never panic and never loop: the bytes come from a hostile guest.
func TestExtractSNIOnHostileInput(t *testing.T) {
	hello := captureClientHello(t, &tls.Config{ServerName: "example.com"})

	cases := [][]byte{
		{},
		{0x16},
		{0x16, 0x03, 0x01},
		{0x16, 0x03, 0x01, 0xff, 0xff}, // record length beyond the cap
		{0x16, 0x03, 0x01, 0x00, 0x00}, // zero-length record
		{0x16, 0x03, 0x01, 0x00, 0x04, 1, 0, 0, 99},                  // handshake claims more than it has
		{0x16, 0x03, 0x01, 0x00, 0x05, 0x01, 0x00, 0x00, 0xff, 0x00}, // length beyond the record
	}
	// Every single-byte corruption of a real ClientHello, to shake out unchecked reads.
	for i := 0; i < len(hello); i += 7 {
		mutated := append([]byte(nil), hello...)
		mutated[i] ^= 0xff
		cases = append(cases, mutated)
	}
	// Every truncation, too.
	for i := 0; i <= len(hello); i += 3 {
		cases = append(cases, hello[:i])
	}

	for i, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked: %v", i, r)
				}
			}()
			_, _, _ = extractSNI(c)
		}()
	}
}
