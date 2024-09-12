// SPDX-License-Identifier: GPL-3.0
// Copyright 2022 Pete Heist

package node

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/heistp/antler/node/metric"
)

// Seq is a packet sequence number.
type Seq uint64

// seqSrc provides a concurrent-safe source of monotonically increasing sequence
// numbers. The zero value is ready to use.
type seqSrc struct {
	seq Seq
	mtx sync.Mutex
}

// Next returns the next sequence number.
func (s *seqSrc) Next() (seq Seq) {
	s.mtx.Lock()
	seq = s.seq
	s.seq++
	s.mtx.Unlock()
	return
}

// PacketFlag represents the flag bits on a packet.
type PacketFlag byte

const (
	// FlagEcho indicates that the packet requests an echo.
	FlagEcho PacketFlag = 1 << iota

	// FlagReply indicates that the packet is a reply to an echo request.
	FlagReply
)

// packetMagic is the 7-byte magic sequence at the beginning of a packet.
var packetMagic = []byte{0xaa, 0x49, 0x7c, 0x06, 0x31, 0xe9, 0x45}

// Packet represents a Packet sent in either direction between a PacketClient
// and PacketServer. Only the header is included in the body of the Packet.
// Padding is added to reach the Packet Length.
type Packet struct {
	PacketHeader

	// Len is the total length of the packet, in bytes, including the header.
	Len int

	// addr is the address the packet is from or to.
	addr net.Addr

	// done, if true, indicates that a packetSender is done.
	done bool

	// err is an error that supersedes the remaining fields.
	err error
}

// PacketHeader represents the header of the packet.
type PacketHeader struct {
	// Flag contains the packet flags.
	Flag PacketFlag

	// Seq is the sequence number assigned by the client.
	Seq Seq

	// Sender is the index of the sender in the client.
	Sender int

	// Flow is the flow identifier, and corresponds to a client and server pair.
	Flow Flow
}

// Write implements io.Writer to "write" from bytes to the packet.
func (p *PacketHeader) Write(b []byte) (n int, err error) {
	if p.Len() > len(b) {
		err = fmt.Errorf("packet header len %d > buf len %d", p.Len(), len(b))
		return
	}
	if !bytes.Equal(b[0:7], packetMagic) {
		err = fmt.Errorf("invalid packet magic: %x", b[0:7])
	}
	p.Flag = PacketFlag(b[7])
	p.Seq = Seq(binary.LittleEndian.Uint64(b[8:16]))
	p.Sender = int(binary.LittleEndian.Uint16(b[16:18]))
	p.Flow = Flow(string(b[19 : 19+b[18]]))
	n = p.Len()
	return
}

// Read implements io.Reader to "read" from the packet to bytes.
func (p *PacketHeader) Read(b []byte) (n int, err error) {
	if len(b) < p.Len() {
		err = fmt.Errorf("buf len %d < packet header len %d", len(b), p.Len())
		return
	}
	if len(p.Flow) > 16 {
		err = fmt.Errorf("flow name %s > 16 characters", p.Flow)
		return
	}
	copy(b, packetMagic)
	b[7] = byte(p.Flag)
	binary.LittleEndian.PutUint64(b[8:16], uint64(p.Seq))
	binary.LittleEndian.PutUint16(b[16:18], uint16(p.Sender))
	b[18] = byte(len(p.Flow))
	copy(b[19:], []byte(p.Flow))
	n = p.Len()
	return
}

// Len returns the length of the header, in bytes.
func (p *PacketHeader) Len() int {
	return len(packetMagic) + 1 + 8 + 2 + 1 + len(p.Flow)
}

// PacketServer is the server used for packet oriented protocols.
type PacketServer struct {
	// ListenAddr is the listen address, as specified to the address parameter
	// in net.ListenPacket (e.g. ":port" or "addr:port").
	ListenAddr string

	// Protocol is the protocol to use (udp, udp4 or udp6).
	Protocol string

	// MaxPacketSize is the maximum size of a received packet.
	MaxPacketSize int

	errc chan error
}

// Run implements runner
func (s *PacketServer) Run(ctx context.Context, arg runArg) (ofb Feedback,
	err error) {
	g := net.ListenConfig{}
	var c net.PacketConn
	if c, err = g.ListenPacket(ctx, s.Protocol, s.ListenAddr); err != nil {
		return
	}
	s.errc = make(chan error)
	s.start(ctx, c, arg.rec)
	arg.cxl <- s
	return
}

// Cancel implements canceler
func (s *PacketServer) Cancel() error {
	return <-s.errc
}

// start starts the main and packet handling goroutines.
func (s *PacketServer) start(ctx context.Context, conn net.PacketConn,
	rec *recorder) {
	ec := make(chan error)
	// main goroutine
	go func() {
		var err error
		defer func() {
			if err != nil {
				s.errc <- err
			}
			close(s.errc)
		}()
		dc := ctx.Done()
		var d bool
		for d {
			select {
			case <-dc:
				dc = nil
				d = true
				err = conn.Close()
			case e, ok := <-ec:
				if !ok {
					d = true
					break
				}
				if dc == nil {
					rec.Logf("post-cancel error: %s", e)
					break
				}
				rec.SendErrore(e)
			}
		}
	}()
	// packet handling goroutine
	go func() {
		var e error
		defer func() {
			if e != nil {
				ec <- e
			}
			close(ec)
		}()
		f := make(map[Flow]struct{})
		var p Packet
		var n int
		var a net.Addr
		b := make([]byte, s.MaxPacketSize)
		d := make(map[Seq]struct{})
		for {
			if n, a, e = conn.ReadFrom(b); e != nil {
				return
			}
			t := metric.Now()
			if _, e = p.Write(b[:n]); e != nil {
				return
			}
			if _, ok := f[p.Flow]; !ok {
				rec.Send(PacketInfo{metric.Tinit, p.Flow, true})
				f[p.Flow] = struct{}{}
			}
			rec.Send(PacketIO{p, t, true, false})
			if p.Flag&FlagEcho != 0 {
				if _, ok := d[p.Seq]; ok {
					continue
				}
				d[p.Seq] = struct{}{}
				p.Flag &= ^FlagEcho
				p.Flag |= FlagReply
				if _, e = p.Read(b); e != nil {
					return
				}
				if _, e = conn.WriteTo(b[:n], a); e != nil {
					return
				}
				rec.Send(PacketIO{p, metric.Now(), true, true})
			}
		}
	}()
}

// PacketClient is the client used for packet oriented protocols.
type PacketClient struct {
	// Addr is the dial address, as specified to the address parameter in
	// net.Dial (e.g. "addr:port").
	Addr string

	// Protocol is the protocol to use (udp, udp4 or udp6).
	Protocol string

	// Flow is the flow identifier for traffic between the client and server.
	Flow Flow

	// MaxPacketSize is the maximum size of a received packet.
	MaxPacketSize int

	Sender []PacketSenders

	// Sockopts provides support for socket options.
	Sockopts

	conn    net.Conn          // connection
	request map[Seq]time.Time // echo request send times
	srtt    time.Duration     // smoothed RTT
	rec     *recorder         // recorder
	timerQ  packetTimerQ      // timer queue
	sender  int               // index of current sender
	seq     Seq               // current sequence number
}

// Run implements runner
func (c *PacketClient) Run(ctx context.Context, arg runArg) (ofb Feedback,
	err error) {
	dl := net.Dialer{Control: c.dialControl}
	if c.conn, err = dl.DialContext(ctx, c.Protocol, c.Addr); err != nil {
		return
	}
	c.request = make(map[Seq]time.Time)
	c.rec = arg.rec
	c.timerQ = packetTimerQ{}
	heap.Init(&c.timerQ)
	c.rec.Send(PacketInfo{metric.Tinit, c.Flow, false})
	r := c.read(arg.rec)
	defer func() {
		c.conn.Close()
		for range r {
		}
	}()
	t0 := time.Now()
	var s PacketSenders
	for c.sender, s = range c.Sender {
		if err = s.packetSender().send(c, t0, nil); err != nil {
			return
		}
	}
	var done bool
	var w <-chan time.Time // wait channel
	var q bool             // timer queue not empty
	var t packetTimer
	for !done {
		// create timer until next packetTimer or until Wait expires
		if w == nil {
			q = c.timerQ.Len() > 0
			if q {
				t = heap.Pop(&c.timerQ).(packetTimer)
				if d := t.at.Sub(time.Now()); d > 0 {
					w = time.After(d)
				} else {
					w = time.After(0)
				}
			} else {
				w = time.After(c.srtt * 3)
			}
		}

		// select on wait channel, read or done
		select {
		case <-w:
			w = nil
			if !q {
				done = true
				break
			}
			c.sender = t.sender
			s := c.Sender[t.sender].packetSender()
			if err = s.send(c, t.at, t.data); err != nil {
				return
			}
		case p, ok := <-r:
			if !ok {
				done = true
				break
			}
			if p.err != nil {
				if err == nil {
					err = p.err
				}
				done = true
				break
			}
			// get smoothed RTT of echo replies
			if p.PacketHeader.Flag&FlagReply != 0 {
				var t time.Time
				var ok bool
				if t, ok = c.request[p.Seq]; ok {
					r := time.Since(t)
					if c.srtt == 0 {
						c.srtt = r
					} else {
						a := 0.125 // RFC 6298
						c.srtt = time.Duration(
							a*float64(r) + (1-a)*float64(c.srtt))
					}
					delete(c.request, p.Seq)
				}
			}
		case <-ctx.Done():
			done = true
		}
	}
	return
}

// read is the entry point for the conn read goroutine.
func (c *PacketClient) read(rec *recorder) (
	rc chan Packet) {
	pc := c.conn.(net.PacketConn)
	rc = make(chan Packet)
	go func() {
		b := make([]byte, c.MaxPacketSize)
		var n int
		var a net.Addr
		var e error
		defer func() {
			if e != nil {
				rc <- Packet{err: e}
			}
			close(rc)
		}()
		for {
			n, a, e = pc.ReadFrom(b)
			now := metric.Now()
			if e != nil {
				break
			}
			var p Packet
			p.addr = a
			if _, e = p.Write(b[:n]); e != nil {
				return
			}
			rec.Send(PacketIO{p, now, false, false})
			rc <- p
		}
	}()
	return
}

// send sends a Packet.
func (c *PacketClient) send(length int, echo bool) (seq Seq, err error) {
	var f PacketFlag
	seq = c.seq
	c.seq++
	if echo {
		f |= FlagEcho
	}
	p := Packet{PacketHeader{f, seq, c.sender, c.Flow}, length, nil, false, nil}
	b := make([]byte, c.MaxPacketSize)
	var n int
	if n, err = p.Read(b); err != nil {
		return
	}
	if p.Len == 0 {
		p.Len = n
	} else if p.Len < n {
		err = fmt.Errorf("requested packet len %d < header len %d",
			p.Len, n)
		return
	}
	if _, err = c.conn.Write(b[:p.Len]); err != nil {
		return
	}
	now := time.Now()
	c.rec.Send(PacketIO{p, metric.Relative(now), false, true})
	if p.PacketHeader.Flag&FlagEcho != 0 {
		c.request[p.Seq] = now
	}
	return
}

// schedule schedules a call to send with the given data.
func (c *PacketClient) schedule(at time.Time, data any) {
	heap.Push(&c.timerQ, packetTimer{c.sender, at, data})
}

// packetTimer schedules an event for PacketClient.
type packetTimer struct {
	sender int
	at     time.Time
	data   any
}

// packetTimerQ is a min-heap for packetTimers, using the heap package.
type packetTimerQ []packetTimer

// Len implements heap.Interface.
func (q packetTimerQ) Len() int {
	return len(q)
}

// Less implements heap.Interface.
func (q packetTimerQ) Less(i, j int) bool {
	return q[i].at.Before(q[j].at)
}

// Swap implements heap.Interface.
func (q packetTimerQ) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
}

// Push implements heap.Interface.
func (q *packetTimerQ) Push(x any) {
	*q = append(*q, x.(packetTimer))
}

// Pop implements heap.Interface.
func (q *packetTimerQ) Pop() any {
	o := *q
	n := len(o)
	t := o[n-1]
	*q = o[:n-1]
	return t
}

// A packetSender can send outgoing packets.  TODO complete doc
type packetSender interface {
	send(client *PacketClient, at time.Time, data any) error
}

// PacketSenders is the union of available packetSender implementations.
type PacketSenders struct {
	Unresponsive *Unresponsive
}

// packetSender returns the only non-nil packetSender implementation.
func (p *PacketSenders) packetSender() packetSender {
	switch {
	case p.Unresponsive != nil:
		return p.Unresponsive
	default:
		panic("no packetSender set in packetSender union")
	}
}

// Unresponsive sends packets on a schedule without regard to any congestion
// signals.
type Unresponsive struct {
	// Wait lists the wait times between packets, which are cycled through
	// either sequentially or randomly (according to RandomWait) until all
	// packets are sent.
	Wait []metric.Duration

	// WaitFirst, if true, indicates to wait before sending the first packet as
	// well.
	WaitFirst bool

	// Length lists the lengths of the packets, which are cycled through either
	// sequentially or randomly (according to RandomLength) until all packets
	// are sent.
	Length []int

	// Duration is how long to send packets.
	Duration metric.Duration

	// Echo, if true, requests mirrored replies from the server.
	Echo bool

	done        time.Time  // start time
	started     bool       // send called at least once
	waitIndex   int        // current index in Wait
	lengthIndex int        // current index in Length
	rand        *rand.Rand // random number source
}

// start implements packetSender.
func (u *Unresponsive) send(client *PacketClient, at time.Time,
	data any) (err error) {
	s := true // send
	if !u.started {
		u.done = at.Add(u.Duration.Duration())
		u.started = true
		if u.WaitFirst {
			s = false
		}
	}
	if s {
		if _, err = client.send(u.nextLength(), u.Echo); err != nil {
			return
		}
	}
	if a := at.Add(u.nextWait()); a.Before(u.done) {
		client.schedule(a, nil)
	}
	return
}

// nextWait returns the next wait time.
func (u *Unresponsive) nextWait() (wait time.Duration) {
	if len(u.Wait) == 0 {
		return
	}
	wait = time.Duration(u.Wait[u.waitIndex])
	if u.waitIndex++; u.waitIndex >= len(u.Wait) {
		u.waitIndex = 0
	}
	return
}

// nextLength returns the next packet length.
func (u *Unresponsive) nextLength() (length int) {
	if len(u.Length) == 0 {
		return
	}
	length = u.Length[u.lengthIndex]
	if u.lengthIndex++; u.lengthIndex >= len(u.Length) {
		u.lengthIndex = 0
	}
	return
}

// PacketInfo contains information for a packet flow.
type PacketInfo struct {
	// Tinit is the base time for the flow's RelativeTime values.
	Tinit time.Time

	// Flow is the flow identifier.
	Flow Flow

	// Server indicates if this is from the server (true) or client (false).
	Server bool
}

// init registers PacketInfo with the gob encoder
func init() {
	gob.Register(PacketInfo{})
}

// Time returns an absolute from a node-relative time.
func (p PacketInfo) Time(r metric.RelativeTime) time.Time {
	return p.Tinit.Add(time.Duration(r))
}

// flags implements message
func (PacketInfo) flags() flag {
	return flagForward
}

// handle implements event
func (p PacketInfo) handle(node *node) {
	node.parent.Send(p)
}

func (p PacketInfo) String() string {
	return fmt.Sprintf("PacketInfo[Tinit:%s Flow:%s]", p.Tinit, p.Flow)
}

// PacketIO is a time series data point that records packet send and receive
// times.
type PacketIO struct {
	// Packet is the packet.
	Packet

	// T is the node-relative time this PacketIO was recorded.
	T metric.RelativeTime

	// Server indicates if this is from the server (true) or client (false).
	Server bool

	// Sent is true for a sent packet, and false for received.
	Sent bool
}

// init registers PacketIO with the gob encoder
func init() {
	gob.Register(PacketIO{})
}

// flags implements message
func (PacketIO) flags() flag {
	return flagForward
}

// handle implements event
func (p PacketIO) handle(node *node) {
	node.parent.Send(p)
}

func (p PacketIO) String() string {
	return fmt.Sprintf("PacketIO[Packet:%v T:%s Sent:%t]",
		p.Packet, p.T, p.Sent)
}
