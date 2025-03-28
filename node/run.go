// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright 2022 Pete Heist

package node

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/heistp/antler/node/metric"
)

//
// Run and related types
//

// Run represents the information needed to coordinate the execution of runners.
// Using the Serial, Parallel and Child fields, Runs may be arranged in a tree
// for sequential, concurrent and child node execution.
//
// Run must be created with valid constraints, i.e. each Run must have exactly
// one of Serial, Parallel, Child or a Runners field set. Run is not safe for
// concurrent use, though Parallel Runs execute safely, concurrently.
type Run struct {
	// Serial lists Runs to be executed sequentially
	Serial Serial

	// Parallel lists Runs to be executed concurrently
	Parallel Parallel

	// Schedule lists Runs to be executed on a schedule.
	Schedule *Schedule

	// ClosedLoopActor alternates a thinking phase with a Run phase.
	ClosedLoopActor *ClosedLoopActor

	// Random executes a random Run from a list of Runs.
	Random *Random

	// Child is a Run to be executed on a child Node
	Child *Child

	// Runners is a union of the available runner implementations.
	//
	// NOTE: In the future, this may be an interface field, if CUE can be made
	// to choose a concrete type without using a field for each runner.
	Runners
}

// run runs the Run.  NOTE Keep validate up to date if fields change.
func (r *Run) run(ctx context.Context, arg runArg, ev chan event) (
	ofb Feedback, ok bool) {
	switch {
	case len(r.Serial) > 0:
		ofb, ok = r.Serial.do(ctx, arg, ev)
	case len(r.Parallel) > 0:
		ofb, ok = r.Parallel.do(ctx, arg, ev)
	case r.Schedule != nil:
		ofb, ok = r.Schedule.do(ctx, arg, ev)
	case r.ClosedLoopActor != nil:
		ofb, ok = r.ClosedLoopActor.do(ctx, arg, ev)
	case r.Random != nil:
		ofb, ok = r.Random.do(ctx, arg, ev)
	case r.Child != nil:
		ofb, ok = r.Child.do(ctx, arg, ev)
	default:
		ofb, ok = r.Runners.do(ctx, arg, ev)
	}
	return
}

// Validate returns an error if the Run fails validation.
func (r *Run) Validate() (err error) {
	var n int
	if len(r.Serial) > 0 {
		if err = r.Serial.validate(); err != nil {
			return
		}
		n++
	}
	if len(r.Parallel) > 0 {
		if err = r.Parallel.validate(); err != nil {
			return
		}
		n++
	}
	if r.Schedule != nil {
		if err = r.Schedule.validate(); err != nil {
			return
		}
		n++
	}
	if r.ClosedLoopActor != nil {
		if err = r.ClosedLoopActor.validate(); err != nil {
			return
		}
		n++
	}
	if r.Random != nil {
		if err = r.Random.validate(); err != nil {
			return
		}
		n++
	}
	if r.Child != nil {
		if err = r.Child.validate(); err != nil {
			return
		}
		n++
	}
	if r.Runners != (Runners{}) {
		if err = r.Runners.validate(); err != nil {
			return
		}
		n++
	}
	// NOTE allow empty Runs for convenience
	if n > 1 {
		err = UnionError{r, n}
	}
	return
}

// Serial is a list of Runs executed sequentially.
type Serial []Run

// do executes the Serial Runs sequentially.
func (s Serial) do(ctx context.Context, arg runArg, ev chan event) (
	ofb Feedback, ok bool) {
	ofb = Feedback{}
	for _, r := range s {
		var f Feedback
		f, ok = r.run(ctx, arg, ev)
		if e := ofb.merge(f); e != nil {
			ok = false
			rr := arg.rec.WithTag(typeBaseName(r))
			ev <- errorEvent{rr.NewErrore(e), false}
		}
		if !ok {
			return
		}
	}
	return
}

// validate returns the first validation error from each of the Runs.
func (s Serial) validate() (err error) {
	for _, r := range s {
		if err = r.Validate(); err != nil {
			return
		}
	}
	return
}

// Parallel is a list of Runs executed concurrently.
type Parallel []Run

// do executes the Parallel Runs concurrently.
func (p Parallel) do(ctx context.Context, arg runArg, ev chan event) (
	ofb Feedback, ok bool) {
	ofb = Feedback{}
	c := make(chan runDone)
	for _, r := range p {
		r := r
		go func() {
			var d runDone
			defer func() {
				c <- d
			}()
			d.run = &r
			d.ofb, d.ok = r.run(ctx, arg, ev)
		}()
	}
	ok = true
	for i := 0; i < len(p); i++ {
		d := <-c
		if e := ofb.merge(d.ofb); e != nil {
			ok = false
			rr := arg.rec.WithTag(typeBaseName(d.run))
			ev <- errorEvent{rr.NewErrore(e), false}
		}
		if !d.ok {
			ok = false
		}
	}
	return
}

// validate returns the first validation error from each of the Runs.
func (p Parallel) validate() (err error) {
	for _, r := range p {
		if err = r.Validate(); err != nil {
			return
		}
	}
	return
}

// Child is a Run to execute on a child Node.
type Child struct {
	// Run is the Run to execute on Node.
	Run

	// Node is the node to execute Run on. It must be a valid, nonzero value.
	Node Node
}

// do executes Child's Run on a child node.
func (r *Child) do(ctx context.Context, arg runArg, ev chan event) (
	ofb Feedback, ok bool) {
	c := arg.child.Get(r.Node)
	rc := make(chan ran, 1)
	c.Run(&r.Run, arg.ifb, rc)
	a := <-rc
	ofb = a.Feedback
	ok = a.OK
	return
}

// validate validates the Child's fields.  NOTE Keep this in sync if any fields
// change.
func (r *Child) validate() (err error) {
	if err = r.Run.Validate(); err != nil {
		return
	}
	if err = r.Node.validate(); err != nil {
		return
	}
	return
}

// RandomRunner is a base type for runners that use a random number generator.
type RandomRunner struct {
	// Seed is the seed for the random number generator. If 0, the current
	// time is used as the seed.
	Seed int64

	// rand is the random number generator.
	rand *rand.Rand
}

// initRandom initializes the random number generator.
func (r *RandomRunner) initRandom() {
	if r.rand == nil {
		if r.Seed == 0 {
			r.Seed = time.Now().UnixNano()
		}
		r.rand = rand.New(rand.NewSource(r.Seed))
	}
}

// Schedule lists Runs to be executed with wait times between each Run.
type Schedule struct {
	// Wait lists the wait Durations to use. If Random is false, the chosen
	// Durations cycle repeatedly through Wait.
	Wait []metric.Duration

	// WaitFirst, if true, indicates to wait before the first Run as well.
	WaitFirst bool

	// Random, if true, indicates to select wait times from Wait randomly.
	// Otherwise, wait times are taken from Wait sequentially.
	Random bool

	// Sequential, if true, indicates to run the Runs in serial.
	Sequential bool

	// Run lists the Runs.
	Run []Run

	// waitIndex is the current index in Wait.
	waitIndex int

	RandomRunner
}

// do executes Schedule's Runs on a schedule.
func (s *Schedule) do(ctx context.Context, arg runArg, ev chan event) (
	ofb Feedback, ok bool) {
	ofb = Feedback{}
	ok = true
	var g, i int
	r := make(chan runDone)
	dc := ctx.Done()
	w := time.After(s.firstWait())
	for (i < len(s.Run) && dc != nil && ok) || g > 0 {
		select {
		case <-w:
			if dc == nil || !ok {
				break
			}
			g++
			go func(run *Run) {
				var d runDone
				defer func() {
					r <- d
				}()
				d.run = run
				d.ofb, d.ok = run.run(ctx, arg, ev)
			}(&s.Run[i])
			if i++; i < len(s.Run) && !s.Sequential {
				w = time.After(s.nextWait())
			}
		case d := <-r:
			g--
			if e := ofb.merge(d.ofb); e != nil {
				ok = false
				rr := arg.rec.WithTag(typeBaseName(d.run))
				ev <- errorEvent{rr.NewErrore(e), false}
				break
			}
			if !d.ok {
				ok = false
			}
			if s.Sequential && dc != nil && ok && i < len(s.Run) {
				w = time.After(s.nextWait())
			}
		case <-dc:
			dc = nil
		}
	}
	return
}

// firstWait returns the first wait time.
func (s *Schedule) firstWait() time.Duration {
	if !s.WaitFirst {
		return 0
	}
	return s.nextWait()
}

// nextWait returns the next wait time.
func (s *Schedule) nextWait() (wait time.Duration) {
	if len(s.Wait) == 0 {
		return
	}
	if s.Random {
		s.initRandom()
		wait = time.Duration(s.Wait[s.rand.Intn(len(s.Wait))])
		return
	}
	wait = time.Duration(s.Wait[s.waitIndex])
	if s.waitIndex++; s.waitIndex >= len(s.Wait) {
		s.waitIndex = 0
	}
	return
}

// validate returns the first validation error from each of the Runs.
func (s *Schedule) validate() (err error) {
	for _, r := range s.Run {
		if err = r.Validate(); err != nil {
			return
		}
	}
	return
}

// ClosedLoopActor is a runner that alternates an exponentially distributed
// thinking phase with a Run phase.
type ClosedLoopActor struct {
	// Duration is the lifetime of the runner.
	Duration metric.Duration

	// ThinkingTime is the mean time of the thinking phase.
	ThinkingTime metric.Duration

	// Run is the Run to execute every time the thinking phase ends.
	Run

	RandomRunner
}

// flowIndexCtxKey is the context key used to store what iteration a
// ClosedLoopActor is on, so that a unique Flow name can be generated for each
// iteration.
type flowIndexCtxKey struct{}

// do alternates waiting for a thinking phase to end with executing the Run.
func (a *ClosedLoopActor) do(ctx context.Context, arg runArg, ev chan event) (
	ofb Feedback, ok bool) {
	ofb = Feedback{}
	ok = true
	var i int

	a.initRandom()

	dc := ctx.Done()
	end := time.After(time.Duration(a.Duration))
	w := time.After(a.nextWait())

	for ok {
		select {
		case <-dc:
			return
		case <-end:
			return
		case <-w:
			ctx := context.WithValue(ctx, flowIndexCtxKey{}, i)
			var fb Feedback
			fb, ok = a.Run.run(ctx, arg, ev)
			w = time.After(a.nextWait())
			i++

			if e := ofb.merge(fb); e != nil {
				ok = false
				rr := arg.rec.WithTag(typeBaseName(a.Run))
				ev <- errorEvent{rr.NewErrore(e), false}
			}
		}
	}
	return
}

// nextWait returns the next wait time, which is a random duration
// exponentially distributed with a mean of ThingkingTime.
func (a *ClosedLoopActor) nextWait() time.Duration {
	return time.Duration(a.rand.ExpFloat64() * float64(a.ThinkingTime))
}

// validate returns the validation error from the Run.
func (a *ClosedLoopActor) validate() (err error) {
	if err = a.Run.Validate(); err != nil {
		return
	}
	return
}

// Random executes a random Run from a list of Runs.
type Random struct {
	// Run is a list of Runs to sample from.
	Run []Run

	// Weights is a list of weights for each Run. If nil, the Runs are
	// selected uniformly. If not nil, the Runs are selected with a
	// probability proportional to the weight of each Run.
	Weights []float64

	cumulativeWeights []float64
	RandomRunner
}

// do executes a random Run from the list of Runs.
func (r *Random) do(ctx context.Context, arg runArg, ev chan event) (
	ofb Feedback, ok bool) {
	r.initRandom()
	run := r.sampleRun()
	ofb, ok = run.run(ctx, arg, ev)
	return
}

// sampleRun returns a random Run from the list of Runs. If Weights is nil,
// a random Run is selected uniformly. If Weights is not nil, a random Run is
// selected with a probability proportional to the weight of each Run.
func (r *Random) sampleRun() (run *Run) {
	if len(r.Run) == 0 {
		return nil
	}
	if r.Weights == nil {
		// when no weights are specified, select a random run uniformly
		run = &r.Run[r.rand.Intn(len(r.Run))]
	} else {
		// lazy init cumulative weights
		if r.cumulativeWeights == nil {
			r.cumulativeWeights = make([]float64, len(r.Weights))
			r.cumulativeWeights[0] = r.Weights[0]
			for i := 1; i < len(r.Weights); i++ {
				r.cumulativeWeights[i] = r.cumulativeWeights[i-1] + r.Weights[i]
			}
		}
		// obtain a random value in the range of [0, sum(weights))
		randVal := r.rand.Float64() * r.cumulativeWeights[len(r.cumulativeWeights)-1]

		// binary search for the first weight that is greater than the random value
		low, high := 0, len(r.cumulativeWeights)-1
		for low < high {
			mid := (low + high) / 2
			if r.cumulativeWeights[mid] <= randVal {
				low = mid + 1
			} else {
				high = mid
			}
		}
		run = &r.Run[low]
	}
	return
}

// validate returns the first validation error from each of the Runs.
// If Weights is not nil, it must have the same number of elements as Run.
func (r *Random) validate() (err error) {
	for _, r := range r.Run {
		if err = r.Validate(); err != nil {
			return
		}
	}
	if r.Weights != nil && len(r.Run) != len(r.Weights) {
		err = fmt.Errorf("number of weights (%d) must match number of runs (%d)",
			len(r.Weights), len(r.Run))
		return
	}
	return
}

// runDone is the result returned by Run's internal goroutines.
type runDone struct {
	run *Run
	ofb Feedback
	ok  bool
}

// Runners is a union of the available runner implementations. Only one of the
// runners may be non-nil.
type Runners struct {
	ResultStream *ResultStream
	Setup        *setup
	Sleep        *Sleep
	SysInfo      *SysInfo
	System       *System
	StreamClient *StreamClient
	StreamServer *StreamServer
	PacketServer *PacketServer
	PacketClient *PacketClient
}

// runner returns the runner.
func (r *Runners) runner() (rr runner) {
	// NOTE not panicking on no fields allows empty runner lists
	rr, _ = r.value()
	//var n int
	//if rr, n = r.value(); n != 1 {
	//	panic(UnionError{r, n}.Error())
	//}
	return
}

// validate returns an error if exactly one field isn't set.
func (r *Runners) validate() (err error) {
	var n int
	var rr runner
	if rr, n = r.value(); n != 1 {
		err = UnionError{r, n}
		return
	}
	if v, ok := rr.(validater); ok {
		err = v.validate()
	}
	return
}

// value returns the last non-nil field, and the number of non-nil fields.
func (r *Runners) value() (rr runner, n int) {
	if r.ResultStream != nil {
		rr = r.ResultStream
		n++
	}
	if r.Setup != nil {
		rr = r.Setup
		n++
	}
	if r.Sleep != nil {
		rr = r.Sleep
		n++
	}
	if r.SysInfo != nil {
		rr = r.SysInfo
		n++
	}
	if r.System != nil {
		rr = r.System
		n++
	}
	if r.StreamClient != nil {
		rr = r.StreamClient
		n++
	}
	if r.StreamServer != nil {
		rr = r.StreamServer
		n++
	}
	if r.PacketClient != nil {
		rr = r.PacketClient
		n++
	}
	if r.PacketServer != nil {
		rr = r.PacketServer
		n++
	}
	return
}

// SetKeyer returns the only non-nil runner implementation as a SetKeyer, or nil
// if it does not exist or is not a SetKeyer.
func (r *Runners) SetKeyer() (sk SetKeyer) {
	var u runner
	if u = r.runner(); u == nil {
		return
	}
	sk, _ = u.(SetKeyer)
	return
}

// do executes the runner.
func (r *Runners) do(ctx context.Context, arg runArg, ev chan event) (
	ofb Feedback, ok bool) {
	var u runner
	if u = r.runner(); u == nil {
		// NOTE not returning an error allows empty runner lists
		//e := arg.rec.NewErrorf("Run has no runner set")
		//ev <- errorEvent{e, false}
		ok = true
		return
	}
	arg.rec = arg.rec.WithTag(typeBaseName(u))
	var err error
	ofb, err = u.Run(ctx, arg)
	if ofb == nil {
		ofb = Feedback{}
	}
	if err != nil {
		ev <- errorEvent{arg.rec.NewErrore(err), false}
		return
	}
	ok = true
	return
}

//
// runner interface and related types
//

// runner is the interface that wraps the Run method. runners are passed to a
// node for execution, and are used for all node calls, from child connection
// setup, to test environment setup, to test clients and servers.
//
// When Context is canceled, runners should return as soon as possible, using
// Context.Err() as the returned error if the cancellation materially affects
// the results.
type runner interface {
	Run(context.Context, runArg) (Feedback, error)
}

// runArg contains the arguments supplied to a runner.
type runArg struct {
	child    *child        // caches child conns
	ifb      Feedback      // incoming Feedback from prior runners
	sockdiag *sockdiag     // access to socket information on Linux
	rec      *recorder     // recorder for logging, data and errors
	cxl      chan canceler // canceler stack
}

// canceler is the interface that wraps the Cancel method. If a runner
// implements canceler and its run method returns successfully, the Cancel
// method will be called before the node exits to perform cleanup operations.
// canceler's are called sequentially, in reverse order from the order in which
// their corresponding runners were called.
type canceler interface {
	Cancel() error
}

// cancelFunc is a function that implements canceler.
type cancelFunc func() error

// Cancel implements canceler
func (c cancelFunc) Cancel() error {
	return c()
}

// SetKeyer is the interface that wraps the SetKey method. If a runner
// implements SetKeyer, it will be called to set a secure random key that's
// global to the antler instance, and thus shared by all nodes.
type SetKeyer interface {
	SetKey([]byte)
}

// Feedback contains key/value pairs, which are returned by runners for use by
// subsequent runners, and are stored in the result Data. Values must be
// supported by gob.
type Feedback map[string]any

// merge merges the given Feedback f2 into this Feedback. An error is returned
// if any of f2's keys already exist in f.
func (f Feedback) merge(f2 Feedback) (err error) {
	for k2, v2 := range f {
		if v, ok := f[k2]; ok {
			err = fmt.Errorf("feedback conflict merging %s=%+v into %s=%+v",
				k2, v2, k2, v)
			return
		}
		f[k2] = v2
	}
	return
}
