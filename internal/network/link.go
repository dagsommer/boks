package network

// The guest link as a byte stream, and the three things that costs.
//
// # Why a stream at all
//
// The link carries one thing: the Ethernet frames of the sandbox's virtio-net device, from
// the VMM to the stack in this process that terminates them and judges what they contain.
// A SOCK_DGRAM UNIX socket carried them until now, in gvisor-tap-vsock's `vfkit` protocol,
// where one datagram is one frame and the kernel keeps the boundaries.
//
// Windows' AF_UNIX has only SOCK_STREAM — there has never been a datagram equivalent — and
// that single dependency was the whole reason the host stack was compiled for Unix only.
// libkrun's `unixstream` backend, which nerdbox selects with `mode=unixstream`, writes each
// frame prefixed by its length as a 4-byte big-endian integer; that is byte for byte
// gvisor-tap-vsock's `qemu` protocol, which this package already links. So the transport
// moves to a stream and the boundary does not move at all: the same switch, the same stack,
// the same forwarder, the same policy decision on every connection.
//
// # What the datagram socket was giving away for free
//
// A datagram is delivered whole or not at all, and its length is the kernel's. On a stream
// the length in front of each frame is written by the *peer*, and three things follow that
// the previous transport never had to think about.
//
//  1. **A declared length is an allocation request.** tap.Switch reads the four bytes and
//     immediately does `make([]byte, size)`, so a peer that says 0xFFFFFFFF asks the Boks
//     supervisor for 4 GiB. Bounded here, before the switch ever sees the prefix.
//  2. **A declared length below an Ethernet header is a crash.** The switch reads the source
//     and destination MACs and the ethertype out of every frame — from a buffer it allocated
//     to exactly the declared length, so a 4-byte frame indexes past the end of it and
//     panics. (The datagram path survived the same runt input only by accident: it read into
//     a 128 KiB buffer it reused, so the slice had capacity nobody had written to. Reading
//     uninitialised bytes rather than crashing is not a property worth keeping either.)
//  3. **A failed write cannot be retried.** tap.Switch retries a write that fails with
//     ENOBUFS, which is right for a datagram socket where a send is all-or-nothing, and
//     wrong for a stream where an unknown prefix of the frame has already gone out: the
//     retry would re-send the whole frame and desynchronise every frame after it. A write
//     error is therefore reported in a form that retry cannot match, so the switch tears the
//     link down instead.
//
// Frames spanning several reads, and several frames arriving in one read, are handled by the
// switch itself — it frames with io.ReadFull over a bufio.Reader — and this wrapper is
// careful not to break that: it changes no byte and no boundary. It only refuses to hand
// over the four bytes of a length prefix until all four have arrived and been checked,
// because the switch allocates the moment it has read them.

import (
	"encoding/binary"
	"fmt"
	"net"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

const (
	// frameHeaderSize is the qemu protocol's framing: one 4-byte big-endian length in
	// front of each Ethernet frame, and nothing else on the wire.
	frameHeaderSize = 4

	// minFrameSize is the smallest frame the switch can parse without reading past the
	// buffer it allocated for it. See point 2 above.
	minFrameSize = header.EthernetMinimumSize

	// maxFrameSize bounds the other end, and is deliberately not the link's MTU. A frame
	// larger than the MTU is a fault, but it is the stack's fault to diagnose, not a
	// reason to drop the link; and if segmentation offload is ever negotiated on this
	// device — Boks asks for no virtio-net features today, so it is not — one "frame"
	// becomes one oversized IP packet. What cannot be exceeded either way is an IP
	// packet's own length field, so an Ethernet header plus 65535 is the bound that stays
	// true whatever the MTU is, and it holds a mis-framed peer to 64 KiB per allocation
	// rather than 4 GiB.
	maxFrameSize = header.EthernetMinimumSize + 65535

	// linkReadBuffer is how much is read from the socket at once. Only 4 bytes are needed
	// for the reader to make progress — everything after a validated prefix is passed
	// straight through — so this size is purely about how many reads a busy link costs.
	linkReadBuffer = 64 * 1024
)

// linkConn is the guest link as the switch sees it: the peer's stream, with the framing
// checked on the way past.
//
// The read side is driven by exactly one goroutine (the switch's receive loop) and the write
// side is serialised by the switch's own write lock, so neither half needs a mutex. They
// share no state.
type linkConn struct {
	net.Conn

	buf []byte
	// r is the first byte not yet handed to the switch, v the first byte not yet
	// validated, and w the first unused byte: buf[r:v] is checked and deliverable,
	// buf[v:w] has arrived and has not been looked at yet.
	r, v, w int
	// need is how much of the frame at the validation point is still to come. Zero means
	// the next bytes are a length prefix, which is unambiguous because a frame is never
	// zero-length.
	need int
	// err is the socket or framing error that ended the link, remembered rather than
	// returned at once so that bytes already validated are delivered first.
	err error
}

func newLinkConn(c net.Conn) *linkConn {
	return &linkConn{Conn: c, buf: make([]byte, linkReadBuffer)}
}

// Read hands the switch only bytes whose framing has been checked.
func (l *linkConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for l.r == l.v {
		if l.err != nil {
			return 0, l.err
		}
		l.fill()
	}
	n := copy(p, l.buf[l.r:l.v])
	l.r += n
	return n, nil
}

// fill reads once from the peer and validates as much of what arrived as it can.
func (l *linkConn) fill() {
	switch {
	case l.r == l.w:
		// Everything read so far has been delivered: start the buffer over rather
		// than walking it to the end and copying.
		l.r, l.v, l.w = 0, 0, 0
	case l.w == len(l.buf):
		// The buffer is full and nothing is deliverable, so buf[v:w] is an incomplete
		// length prefix — three bytes at most. Moving it to the front always leaves
		// room to read, which is what guarantees this loop makes progress.
		copy(l.buf, l.buf[l.r:l.w])
		l.w -= l.r
		l.v -= l.r
		l.r = 0
	}

	n, err := l.Conn.Read(l.buf[l.w:])
	l.w += n
	if verr := l.validate(); verr != nil {
		l.err = verr
		return
	}
	l.err = err
}

// validate walks the frames that have arrived and refuses a length the switch must not be
// allowed to act on. It moves no bytes: the stream the switch reads is the stream the peer
// wrote, or the link ends.
func (l *linkConn) validate() error {
	for l.v < l.w {
		if l.need > 0 {
			// Inside a frame's payload. Nothing here needs checking — the frame's
			// length is already known to be sane, and its contents are the guest's
			// to choose; the stack judges those.
			n := l.w - l.v
			if n > l.need {
				n = l.need
			}
			l.v += n
			l.need -= n
			continue
		}
		if l.w-l.v < frameHeaderSize {
			// An incomplete length prefix. None of it may be delivered yet: the
			// switch allocates as soon as it has read four bytes.
			return nil
		}
		// int is 32 bits on a 32-bit build, where anything above 2 GiB arrives here
		// negative. The lower bound catches that as well as it catches a runt frame.
		size := int(binary.BigEndian.Uint32(l.buf[l.v : l.v+frameHeaderSize]))
		if size < minFrameSize || size > maxFrameSize {
			return fmt.Errorf("network: the link announced a %d-byte frame, outside the %d-%d bytes "+
				"an Ethernet frame can be: the peer is not speaking this protocol", size, minFrameSize, maxFrameSize)
		}
		l.v += frameHeaderSize
		l.need = size
	}
	return nil
}

// Write passes the switch's framed write through, and converts a failure into an error the
// switch's datagram-era retry cannot match. See point 3 at the top of this file.
func (l *linkConn) Write(p []byte) (int, error) {
	n, err := l.Conn.Write(p)
	if err != nil {
		return n, linkWriteError{err: err}
	}
	return n, nil
}

// linkWriteError deliberately does not implement Unwrap. That is the whole point of it: a
// wrapped syscall.ENOBUFS would still match errors.Is in tap.Switch's retry loop, and
// retrying a stream write that has already put part of a frame on the wire corrupts every
// frame after it. Failing the link is the only safe answer, and this is what makes the
// switch take it.
type linkWriteError struct{ err error }

func (e linkWriteError) Error() string {
	return "network: writing to the guest link: " + e.err.Error()
}
