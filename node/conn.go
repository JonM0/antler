// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2022 Pete Heist

package node

import (
	"fmt"
	"sync"
)

// txBufLen is the length of the send goroutine's buffered channel.
const txBufLen = 16

// conn is a connection to another node. conn must be created with newConn, and
// is safe for concurrent use. All methods except Close are asynchronous, with
// errors sent to the event channel passed to the start method.
//
// To end the conn, callers must call Cancel, Canceled or Close. After all
// goroutines have completed and the underlying transport is closed, connDone
// will be sent on the event channel.
type conn struct {
	mtx      sync.Mutex
	tr       transport     // underlying transport
	to       Node          // peer node
	tq       chan any      // send queue
	tx       chan message  // send goroutine channel
	io       int           // I/O goroutine count
	rpc      map[runID]run // active RPC calls
	id       runID         // ID for next Run call
	canceled bool          // true if conn is canceled
}

// newConn returns a new conn for the given underlying conn.
func newConn(tr transport, to Node) *conn {
	return &conn{
		sync.Mutex{},                 // mtx
		tr,                           // tr
		to,                           // to
		make(chan any),               // tq
		make(chan message, txBufLen), // tx
		0,                            // io
		make(map[runID]run),          // run
		0,                            // id
		false,                        // canceled
	}
}

// Run asynchronously sends the given Run for remote execution. If the conn was
// canceled or closed, Run will fail immediately, thus, callers must ensure that
// ranc has a buffer size of at least 1.
func (c *conn) Run(r *Run, ifb Feedback, ranc chan ran) {
	c.mtx.Lock()
	defer func() {
		c.id++
		c.mtx.Unlock()
	}()
	if c.canceled {
		ranc <- ran{c.id, Feedback{}, false, c.to}
		return
	}
	u := run{c.id, r, ifb, c.to, ranc}
	c.rpc[c.id] = u
	c.tq <- u
}

// Send sends a message. If the conn was canceled or closed, the message is
// dropped.
func (c *conn) Send(m message) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.canceled {
		return
	}
	c.tq <- m
}

// Cancel sends a cancel message and "cancels" the conn. If the call was
// canceled or closed, this call does nothing.
func (c *conn) Cancel() {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.canceled {
		return
	}
	c.canceled = true
	c.tq <- cancel{}
}

// Canceled sends a canceled message and "cancels" the conn. If the call was
// canceled or closed, this call does nothing.
func (c *conn) Canceled() {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.canceled {
		return
	}
	c.canceled = true
	c.tq <- canceled{}
}

// Stream uses the given ResultStream to select which messages will be sent
// immediately (streamed) or buffered. If the call was canceled or closed, this
// call does nothing.
func (c *conn) Stream(s *ResultStream) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.canceled {
		return
	}
	c.tq <- s
}

// failRPC causes all RPCs to return a failure. This method is for internal use,
// and must be called with c.mtx locked.
func (c *conn) failRPC() {
	for i, r := range c.rpc {
		r.ran <- ran{r.ID, Feedback{}, false, c.to}
		delete(c.rpc, i)
	}
}

// start starts the I/O goroutines. The caller must read from the event channel
// until connDone is received.
func (c *conn) start(ev chan<- event) {
	go c.buffer()
	c.io += 2
	go c.send(ev)
	go c.receive(ev)
}

// buffer receives messages and stream filters from the tq channel until closed,
// or a final message is received, buffering messages as necessary and writing
// them to the tx channel. After all messages have been sent, tx is closed.
func (c *conn) buffer() {
	defer close(c.tx)
	var s *ResultStream
	t := make([]message, 0, 1024)
	b := make([]message, 0, 8192)
	txc := func() chan message {
		if len(t) > 0 {
			return c.tx
		}
		return nil
	}
	txm := func() message {
		if len(t) > 0 {
			return t[0]
		}
		return nil
	}
	release := func() {
		for _, p := range b {
			t = append(t, p)
		}
		b = b[:0]
	}
	tq := c.tq
	for tq != nil || txc() != nil {
		select {
		case a := <-tq:
			if a == nil {
				tq = nil
				release()
				break
			}
			var m message
			switch v := a.(type) {
			case message:
				if v.flags()&flagPush != 0 || (s != nil && s.accept(v)) {
					m = v
					break
				}
				b = append(b, v)
			case *ResultStream:
				s = v
				bb := make([]message, 0, len(b)+8192)
				for _, m := range b {
					if s.accept(m) {
						t = append(t, m)
					} else {
						bb = append(bb, m)
					}
				}
				b = bb
			}
			if m != nil {
				if m.flags()&flagFinal != 0 {
					tq = nil
					release()
				}
				t = append(t, m)
			}
		case txc() <- txm():
			t = t[1:]
		}
	}
}

// send sends messages from the tx channel to the transport, until tx is closed.
// After the first error, the tx channel is drained and messages dropped.
func (c *conn) send(ev chan<- event) {
	defer c.ioDone(ev)
	defer func() {
		for range c.tx {
		}
	}()
	for m := range c.tx {
		if e := c.tr.Send(m); e != nil {
			e = fmt.Errorf("send error to '%s': %w", c.to, e)
			ev <- errorEvent{e, true}
			return
		}
	}
}

// receive receives messages from the transport.
func (c *conn) receive(ev chan<- event) {
	defer c.ioDone(ev)
	for {
		m, e := c.tr.Receive()
		if e != nil {
			e = fmt.Errorf("receive error from '%s': %w", c.to, e)
			ev <- errorEvent{e, true}
			return
		}
		if m == nil {
			e = fmt.Errorf("nil message received from %s", c.to)
			ev <- errorEvent{e, true}
			return
		}
		if e := c.received(m, ev); e != nil {
			ev <- errorEvent{e, true}
			return
		}
		if m.flags()&flagFinal != 0 {
			return
		}
	}
}

// received is called by the receive goroutine to handle received messages.
func (c *conn) received(m message, ev chan<- event) (err error) {
	switch v := m.(type) {
	case ran:
		c.mtx.Lock()
		defer c.mtx.Unlock()
		if r, ok := c.rpc[v.ID]; ok {
			v.from = c.to
			r.ran <- v
			delete(c.rpc, v.ID)
		}
	case run:
		v.to = c.to
		ev <- v
	case event:
		ev <- v
	case canceled:
		c.mtx.Lock()
		defer c.mtx.Unlock()
		c.failRPC()
	default:
		err = fmt.Errorf("received unknown message type from '%s': %T", c.to, v)
	}
	return
}

// ioDone is called when either the send() or receive() goroutines are done.
// When both are done, the conn is closed and the connDone event is sent.
func (c *conn) ioDone(ev chan<- event) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.io--; c.io == 0 {
		c.failRPC()
		close(c.tq)
		if e := c.tr.Close(); e != nil {
			e = fmt.Errorf("close error for '%s': %w", c.to, e)
			ev <- errorEvent{e, false}
		}
		ev <- connDone{c.to}
	}
}

// connDone is sent after a conn's goroutines are done and the underlying
// transport is closed.
type connDone struct {
	to Node
}

// handle implements event
func (c connDone) handle(node *node) {
	if c.to == ParentNode {
		node.parentDone = true
		return
	}
	node.child.Delete(c.to)
}

// child provides a concurrent-safe, one to one cache of conns for child Nodes.
type child struct {
	m   map[Node]*conn
	ev  chan<- event
	mtx sync.Mutex
}

// newChild returns a new instance of child.
func newChild(ev chan<- event) *child {
	return &child{
		make(map[Node]*conn),
		ev,
		sync.Mutex{},
	}
}

// Launch launches the given Node and saves it in the cache.
func (c *child) Launch(n Node, log logFunc) (
	conn *conn, err error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	var t transport
	if t, err = n.launch(log); err != nil {
		return
	}
	conn = newConn(t, n)
	conn.start(c.ev)
	c.m[n] = conn
	return
}

// Get returns a conn for a Node from the cache. If Launch was not successfully
// called for the Node beforehand, nil will be returned.
func (c *child) Get(n Node) (conn *conn) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	conn = c.m[n]
	return
}

// Delete removes the conn for the given Node, if it exists.
func (c *child) Delete(n Node) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	delete(c.m, n)
}

// Count returns the number of children in the cache.
func (c *child) Count() int {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	return len(c.m)
}

// Cancel cancels all of the children in the cache.
func (c *child) Cancel() {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	for _, c := range c.m {
		c.Cancel()
	}
}
