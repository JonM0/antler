// SPDX-License-Identifier: GPL-3.0
// Copyright 2022 Pete Heist

package node

import (
	"context"
	"encoding/gob"
	"fmt"
	"strings"
	"time"
)

//
// message and related types
//

// A message can be sent and received by a conn.
type message interface {
	flags() flag
}

// flag is a bitmask of binary attributes on a message.
type flag uint32

const (
	flagFinal   flag = 1 << iota // final message for this direction of a conn
	flagPush                     // do not buffer or delay the message
	flagForward                  // forward directly from child to parent conn
)

//
// run and ran messages
//

// runID is the identifier for run's.
type runID uint32

// run is a message that executes a Run.
type run struct {
	ID       runID
	Run      *Run
	Feedback Feedback
	conn     *conn
	ran      chan ran
}

func init() {
	gob.Register(run{})
}

// handle implements event
func (r run) handle(n *node) {
	if n.state > stateRun {
		n.rec.Logf("dropping run in state %s: %s", n.state, r)
		return
	}
	n.runc <- r
	return
}

// flags implements message
func (r run) flags() flag {
	return flagPush
}

func (r run) String() string {
	return fmt.Sprintf("run[id:%d fb:%s run:%+v]", r.ID, r.Feedback, r.Run)
}

// ran is the reply message to run.
type ran struct {
	ID       runID
	Feedback Feedback
	OK       bool
	conn     *conn
}

func init() {
	gob.Register(ran{})
}

// flags implements message
func (r ran) flags() flag {
	return flagPush
}

func (r ran) String() string {
	return fmt.Sprintf("ran[id:%d fb:%s ok:%t]", r.ID, r.Feedback, r.OK)
}

//
// setup runner
//

// setup is an internal runner used to recursively launch child nodes. It must
// run before any other Runs.
type setup struct {
	ID       runID
	Children tree
	Exes     exes
}

func init() {
	gob.Register(setup{})
}

// Run implements runner
//
// Run launches and runs setup on child nodes, recursively through the node
// tree. After successful setup, the node is ready to execute Run's.
func (s setup) Run(ctx context.Context, chl *child, ifb Feedback,
	rec *recorder, cxl chan canceler) (ofb Feedback, err error) {
	if err = repo.AddSource(s.Exes); err != nil {
		return
	}
	r := rec.WithTag("launch")
	rc := make(chan ran, len(s.Children))
	for n, t := range s.Children {
		cr := rec.WithTag(fmt.Sprintf("launch-%s", n))
		var c *conn
		if c, err = chl.Launch(n, cr.Logf); err != nil {
			return
		}
		var x exes
		if x, err = newExes(repo, t.Platforms()); err != nil {
			return
		}
		x.Remove(n.Platform)
		s := &setup{0, t, x}
		c.Run(&Run{Runners: Runners{Setup: s}}, ifb, rc)
	}
	for i := 0; i < chl.Count(); i++ {
		select {
		case a := <-rc:
			if !a.OK {
				err = r.NewErrorf("setup on child node %s failed", a.conn.to)
				return
			}
		case <-ctx.Done():
			err = ctx.Err()
			return
		}
	}
	return
}

// flags implements message
func (s setup) flags() flag {
	return flagPush
}

func (s setup) String() string {
	return fmt.Sprintf("setup[id:%d]", s.ID)
}

//
// cancel and canceled
//

// cancel is sent to cancel the node at any stage of its operation, including
// after normal execution. It is the final message sent from parent to child.
type cancel struct {
	Reason string
}

func init() {
	gob.Register(cancel{})
}

// handle implements event
func (c cancel) handle(node *node) {
	switch node.state {
	case stateRun:
		if c.Reason != "" {
			node.rec.Logf("canceled for reason: %s", c.Reason)
		}
		node.cancel = true
	default:
		if c.Reason != "" {
			node.rec.Logf("ignoring cancel request for reason '%s' (state: %s)",
				c.Reason, node.state)
		}
	}
}

// flags implements message
func (c cancel) flags() flag {
	return flagPush | flagFinal
}

func (c cancel) String() string {
	return "cancel"
}

// canceled is the final message sent from child to parent.
type canceled struct{}

func init() {
	gob.Register(canceled{})
}

// flags implements message
func (c canceled) flags() flag {
	return flagPush | flagFinal
}

func (c canceled) String() string {
	return "canceled"
}

//
// Error and related types
//

// Error represents an unrecoverable error that occurred on a node.
type Error struct {
	Time    time.Time // the node time that the error occurred
	NodeID  string    // the node ID
	Tag     string    // a string for error categorization
	Message string    // the error text
}

func init() {
	gob.Register(Error{})
}

// errorTag is used when creating the DataPoint Series for Errors.
const errorTag = ".error."

// DataPoint implements DataPointer
func (e Error) DataPoint() DataPoint {
	b := strings.Builder{}
	b.Grow(len(e.NodeID) + len(errorTag) + len(e.Tag))
	b.WriteString(e.NodeID)
	b.WriteString(errorTag)
	b.WriteString(e.Tag)
	s := Series(b.String())
	return DataPoint{s, Time{e.Time}, e.Message}
}

// flags implements message
func (e Error) flags() flag {
	return flagPush
}

func (e Error) Error() string {
	return fmt.Sprintf("%s %s %s: %s", e.Time.Format(readableTimeFormat),
		e.NodeID, e.Tag, e.Message)
}

func (e Error) String() string {
	return fmt.Sprintf("Error[%s]", e.Error())
}

// ErrorFactory provides methods to create and return Errors.
type ErrorFactory struct {
	nodeID string // the Error's NodeID
	tag    string // the Error's Tag
}

// NewError returns a new Error with the given message.
func (f ErrorFactory) NewError(message string) Error {
	t := time.Now()
	return Error{t, f.nodeID, f.tag, message}
}

// NewErrore returns an Error from the given error. If the given error is
// already an Error, the existing error is returned.
func (f ErrorFactory) NewErrore(err error) Error {
	t := time.Now()
	if e, ok := err.(Error); ok {
		return e
	}
	return Error{t, f.nodeID, f.tag, err.Error()}
}

// NewErrorf returns an Error with its Message formatted with prinf style args.
func (f ErrorFactory) NewErrorf(format string, a ...interface{}) Error {
	t := time.Now()
	return Error{t, f.nodeID, f.tag, fmt.Sprintf(format, a...)}
}
