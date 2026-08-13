package network

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// The framing tests. A datagram socket gave the link message boundaries for free; a stream
// does not, and a frame-boundary bug here is a packet-corruption bug in the data path — one
// that would show up as a guest whose network is subtly, intermittently wrong rather than as
// a crash. So the properties are pinned against the real switch, not against a re-reading of
// the wrapper's own logic: what these drive is tap.Switch, over the same protocol constant
// the stack uses, and what they check is the frames it delivered.

const (
	testGatewayMAC = "5a:94:ef:e4:0c:01"
	testGuestMAC   = "5a:94:ef:e4:0c:02"
)

// ethFrame builds a frame from the guest to the gateway with the given payload.
func ethFrame(payload []byte) []byte {
	frame := make([]byte, header.EthernetMinimumSize+len(payload))
	dst, _ := net.ParseMAC(testGatewayMAC)
	src, _ := net.ParseMAC(testGuestMAC)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(src),
		DstAddr: tcpip.LinkAddress(dst),
		Type:    header.IPv4ProtocolNumber,
	})
	copy(frame[header.EthernetMinimumSize:], payload)
	return frame
}

// broadcastFrame is the smallest thing that is a frame at all: an Ethernet header and
// nothing else, addressed to everyone.
func broadcastFrame() []byte {
	frame := make([]byte, header.EthernetMinimumSize)
	src, _ := net.ParseMAC(testGuestMAC)
	header.Ethernet(frame).Encode(&header.EthernetFields{
		SrcAddr: tcpip.LinkAddress(src),
		DstAddr: header.EthernetBroadcastAddress,
		Type:    header.IPv4ProtocolNumber,
	})
	return frame
}

// framed prefixes a frame with its length the way libkrun's unixstream backend does.
func framed(frame []byte) []byte {
	out := make([]byte, frameHeaderSize+len(frame))
	binary.BigEndian.PutUint32(out, uint32(len(frame)))
	copy(out[frameHeaderSize:], frame)
	return out
}

// declaring builds a length prefix for a frame that is not there, which is the whole class
// of input the peer can make up.
func declaring(size uint32) []byte {
	out := make([]byte, frameHeaderSize)
	binary.BigEndian.PutUint32(out, size)
	return out
}

// captureDevice stands in for the host stack's link endpoint and records what the switch
// delivered to it.
type captureDevice struct {
	mu  sync.Mutex
	got [][]byte
}

func (d *captureDevice) DeliverNetworkPacket(_ tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	payload := pkt.Data().ToBuffer()
	d.got = append(d.got, payload.Flatten())
}

func (d *captureDevice) LinkAddress() tcpip.LinkAddress {
	mac, _ := net.ParseMAC(testGatewayMAC)
	return tcpip.LinkAddress(mac)
}

func (d *captureDevice) IP() string { return DefaultGatewayIP }

func (d *captureDevice) delivered() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.got
}

// chunkedConn replays a recorded stream, handing over at most chunk bytes per Read.
//
// That is the point of it: a stream socket may split a frame across any number of reads and
// may deliver several frames in one, and neither is an error condition — it is what the
// kernel does under load. A conn that always returned whole frames would test nothing.
type chunkedConn struct {
	data  []byte
	chunk int
	pos   int
}

func (c *chunkedConn) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > c.chunk {
		n = c.chunk
	}
	if n > len(c.data)-c.pos {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

func (c *chunkedConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *chunkedConn) Close() error                     { return nil }
func (c *chunkedConn) LocalAddr() net.Addr              { return testAddr{} }
func (c *chunkedConn) RemoteAddr() net.Addr             { return testAddr{} }
func (c *chunkedConn) SetDeadline(time.Time) error      { return nil }
func (c *chunkedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *chunkedConn) SetWriteDeadline(time.Time) error { return nil }

type testAddr struct{}

func (testAddr) Network() string { return "unix" }
func (testAddr) String() string  { return "link-test" }

// runSwitch feeds a recorded stream through a linkConn into a real tap.Switch, and returns
// what the switch made of it.
func runSwitch(t *testing.T, stream []byte, chunk int) (*captureDevice, error) {
	t.Helper()
	device := &captureDevice{}
	sw := tap.NewSwitch(false)
	sw.Connect(device)
	conn := newLinkConn(&chunkedConn{data: stream, chunk: chunk})
	return device, sw.Accept(context.Background(), conn, types.QemuProtocol)
}

// TestTheSwitchSeesExactlyTheFramesThePeerWrote is the framing property, checked at every
// read boundary that matters: one byte at a time, boundaries that fall inside a length
// prefix, boundaries that fall inside a payload, and the whole stream in a single read.
func TestTheSwitchSeesExactlyTheFramesThePeerWrote(t *testing.T) {
	frames := [][]byte{
		// The minimum: an Ethernet header and no payload.
		broadcastFrame(),
		ethFrame([]byte("first")),
		// A full 1500-byte MTU frame.
		ethFrame(bytes.Repeat([]byte{0xa5}, 1486)),
		// A one-byte payload, so every length prefix after it is oddly aligned.
		ethFrame([]byte{0x00}),
		ethFrame(bytes.Repeat([]byte{0x5a}, 3000)),
	}
	var stream []byte
	for _, f := range frames {
		stream = append(stream, framed(f)...)
	}

	for _, chunk := range []int{1, 2, 3, 4, 5, 7, 13, 64, 1500, len(stream)} {
		t.Run(strconv.Itoa(chunk)+" bytes per read", func(t *testing.T) {
			device, err := runSwitch(t, stream, chunk)
			if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") {
				t.Fatalf("the link ended with %v, want the peer's EOF", err)
			}
			got := device.delivered()
			if len(got) != len(frames) {
				t.Fatalf("the switch saw %d frames, want %d: the framing does not survive %d-byte reads",
					len(got), len(frames), chunk)
			}
			for i := range frames {
				// The switch trims the Ethernet header before delivering.
				want := frames[i][header.EthernetMinimumSize:]
				if !bytes.Equal(got[i], want) {
					t.Fatalf("frame %d came out as %d bytes %x, want %d bytes %x",
						i, len(got[i]), got[i], len(want), want)
				}
			}
		})
	}
}

// TestFramingSurvivesArbitraryFrameAndReadSizes is the same property against sizes nobody
// chose. A hand-picked table proves the cases its author thought of; the boundary bugs in a
// stream reader are the ones nobody thinks of, so the sizes here are random and the seed is
// fixed so a failure can be reproduced.
func TestFramingSurvivesArbitraryFrameAndReadSizes(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 20; round++ {
		var frames [][]byte
		var stream []byte
		for i := 0; i < 25; i++ {
			payload := make([]byte, rng.Intn(2000))
			rng.Read(payload)
			frame := ethFrame(payload)
			frames = append(frames, frame)
			stream = append(stream, framed(frame)...)
		}
		chunk := 1 + rng.Intn(4096)

		device, err := runSwitch(t, stream, chunk)
		if err == nil || !strings.Contains(err.Error(), "EOF") {
			t.Fatalf("round %d: the link ended with %v, want the peer's EOF", round, err)
		}
		got := device.delivered()
		if len(got) != len(frames) {
			t.Fatalf("round %d (%d-byte reads): the switch saw %d frames, want %d",
				round, chunk, len(got), len(frames))
		}
		for i := range frames {
			if !bytes.Equal(got[i], frames[i][header.EthernetMinimumSize:]) {
				t.Fatalf("round %d (%d-byte reads): frame %d is corrupt", round, chunk, i)
			}
		}
	}
}

// TestALengthNoFrameCouldHaveEndsTheLink is the check the datagram socket never needed.
//
// The length in front of each frame is written by the peer, and the switch acts on it twice
// before anything has validated it: it allocates a buffer of exactly that size, and it then
// reads six bytes of source MAC out of the front of that buffer. So a peer that announces
// 4 GiB asks the Boks supervisor for 4 GiB, and a peer that announces four bytes makes it
// index past the end of a four-byte slice. Neither may reach the switch.
func TestALengthNoFrameCouldHaveEndsTheLink(t *testing.T) {
	tests := []struct {
		name string
		size uint32
	}{
		{"a zero-length frame", 0},
		{"a frame too short to hold a MAC address", 4},
		{"one byte short of an Ethernet header", header.EthernetMinimumSize - 1},
		{"one byte over the largest frame an IP packet fits in", maxFrameSize + 1},
		{"the whole address space", 0xffffffff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A good frame first, so the failure cannot be "it never worked".
			stream := append(framed(ethFrame([]byte("real"))), declaring(tc.size)...)
			// Bytes after the prefix, in case anything is tempted to wait for them.
			stream = append(stream, bytes.Repeat([]byte{0x41}, 64)...)

			device, err := runSwitch(t, stream, 7)
			if err == nil {
				t.Fatal("the switch accepted a length no frame could have")
			}
			if !strings.Contains(err.Error(), "not speaking this protocol") {
				t.Errorf("the link ended with %v, which is not the framing refusal", err)
			}
			if len(device.delivered()) != 1 {
				t.Errorf("the switch saw %d frames, want the 1 good one before the bad length",
					len(device.delivered()))
			}
		})
	}
}

// TestAFrameAtTheBoundIsCarried: the bound has to be a bound on nonsense, not on traffic.
// The largest thing that can legitimately arrive is one IP packet's worth, and it must go
// through — a limit that dropped the link on a legal frame would be worse than none.
func TestAFrameAtTheBoundIsCarried(t *testing.T) {
	frame := ethFrame(make([]byte, maxFrameSize-header.EthernetMinimumSize))
	if len(frame) != maxFrameSize {
		t.Fatalf("built a %d-byte frame, want %d", len(frame), maxFrameSize)
	}
	device, err := runSwitch(t, framed(frame), 4096)
	if err == nil || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("the link ended with %v, want the peer's EOF", err)
	}
	if got := device.delivered(); len(got) != 1 || len(got[0]) != maxFrameSize-header.EthernetMinimumSize {
		t.Fatalf("the largest legal frame did not arrive intact: %d frames", len(got))
	}
}

// TestAnIncompletePrefixIsNeverActedOn: the switch allocates the moment it has four bytes,
// so a length that has only half arrived must not be handed over half-read. Ending the
// stream inside a prefix is the case that catches a reader which passes bytes through and
// validates afterwards.
func TestAnIncompletePrefixIsNeverActedOn(t *testing.T) {
	stream := append(framed(ethFrame([]byte("real"))), 0xff, 0xff) // half a 4 GiB claim
	device, err := runSwitch(t, stream, 1)
	if err == nil {
		t.Fatal("a truncated length prefix was treated as a frame")
	}
	if len(device.delivered()) != 1 {
		t.Errorf("the switch saw %d frames, want the 1 complete one", len(device.delivered()))
	}
}

// TestAFailedWriteIsNotRetried pins the reason linkWriteError exists.
//
// tap.Switch retries a write that fails with ENOBUFS, which is correct for a datagram socket
// where a send either happens whole or not at all. On a stream an unknown prefix of the frame
// has already gone out, so re-sending it from the start would desynchronise every frame after
// it — silently, and for the rest of the sandbox's life. The retry must not match.
func TestAFailedWriteIsNotRetried(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	link := newLinkConn(client)
	_ = client.Close()

	_, err := link.Write(framed(broadcastFrame()))
	if err == nil {
		t.Fatal("writing to a closed link succeeded")
	}
	if errors.Is(err, syscall.ENOBUFS) {
		t.Error("a failed stream write reports ENOBUFS; the switch would retry it and corrupt the stream")
	}
	if !strings.Contains(err.Error(), "writing to the guest link") {
		t.Errorf("the error does not say what failed: %v", err)
	}
}

// TestClosingTheLinkReleasesABlockedWrite is the backpressure property.
//
// A stream write blocks when the peer stops reading, which is the right behaviour — it is
// backpressure rather than the datagram path's spin on ENOBUFS, and nothing is forwarded to
// a guest that is not listening. What it must not be is unbreakable: teardown closes the
// connection, and that has to release the writer, or a sandbox with a wedged VMM would never
// finish stopping. net.Pipe is the sharpest version of a peer that never reads.
func TestClosingTheLinkReleasesABlockedWrite(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	link := newLinkConn(client)

	done := make(chan error, 1)
	go func() {
		_, err := link.Write(framed(broadcastFrame()))
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("the write did not block against a peer that never reads: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	_ = client.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("the blocked write reported success after the link was closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the link did not release the blocked write; teardown would hang")
	}
}
