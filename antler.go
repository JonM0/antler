// SPDX-License-Identifier: GPL-3.0
// Copyright 2022 Pete Heist

// Package antler contains types for running the Antler application.

package antler

import (
	"context"
	"encoding/gob"
	"errors"
	"io"
	"io/fs"

	"cuelang.org/go/cue/load"
	"github.com/heistp/antler/node"
)

// dataChanBufSize is used as the buffer size for data channels.
const dataChanBufSize = 64

// Run runs an Antler Command.
func Run(ctx context.Context, cmd Command) error {
	return cmd.run(ctx)
}

// A Command is an Antler command.
type Command interface {
	run(context.Context) error
}

// RunCommand runs tests and reports.
type RunCommand struct {
	// Force re-runs the test and overwrites any existing data.
	Force bool

	// Filter selects which tests to run.
	Filter TestFilter

	// SkippedFiltered is called when a test was skipped because it was rejected
	// by the Filter.
	SkippedFiltered func(test *Test)

	// SkippedDataFileExists is called when a test was skipped because there's
	// already an output data file for it and RunCommand.Force is false.
	SkippedDataFileExists func(test *Test, path string)
}

// run implements command
func (r *RunCommand) run(ctx context.Context) (err error) {
	var c *Config
	if c, err = LoadConfig(&load.Config{}); err != nil {
		return
	}
	err = c.Run.do(ctx, r, reporterStack{})
	return
}

// do implements doer
func (c *RunCommand) do(ctx context.Context, test *Test, rst reporterStack) (
	err error) {
	ctx, x := context.WithCancelCause(ctx)
	if c.Filter != nil && !c.Filter.Accept(test) {
		c.SkippedFiltered(test)
		return
	}
	var w io.WriteCloser
	if w, err = test.DataWriter(c.Force); err != nil {
		switch e := err.(type) {
		case *FileExistsError:
			if c.SkippedDataFileExists != nil {
				c.SkippedDataFileExists(test, e.Path)
			}
			err = nil
			return
		case *NoDataFileError:
			err = nil
		default:
			return
		}
	}
	if w != nil {
		rst.push([]reporter{saveData{w}})
	}
	d := make(chan any, dataChanBufSize)
	defer rst.pop()
	go node.Do(ctx, &test.Run, &exeSource{}, d)
	err = rst.tee(x, d, test)
	return
}

// ReportCommand runs reports.
type ReportCommand struct {
	// Filter selects which tests to run.
	Filter TestFilter

	// SkippedFiltered is called when a test was skipped because it was rejected
	// by the Filter.
	SkippedFiltered func(test *Test)

	// SkippedNoDataFile is called when a report was skipped because the Test's
	// DataFile field is empty.
	SkippedNoDataFile func(test *Test)

	// SkippedNotFound is called when a report was skipped because the data file
	// needed to run it doesn't exist.
	SkippedNotFound func(test *Test, path string)
}

// run implements command
func (r *ReportCommand) run(ctx context.Context) (err error) {
	var c *Config
	if c, err = LoadConfig(&load.Config{}); err != nil {
		return
	}
	err = c.Run.do(ctx, r, reporterStack{})
	return
}

// do implements doer
func (c *ReportCommand) do(ctx context.Context, test *Test, rst reporterStack) (
	err error) {
	ctx, x := context.WithCancelCause(ctx)
	if c.Filter != nil && !c.Filter.Accept(test) {
		c.SkippedFiltered(test)
		return
	}
	var r io.ReadCloser
	if r, err = test.DataReader(); err != nil {
		if _, ok := err.(*NoDataFileError); ok {
			if c.SkippedNoDataFile != nil {
				c.SkippedNoDataFile(test)
			}
			err = nil
			return
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return
		}
		e := err.(*fs.PathError)
		if c.SkippedNotFound != nil {
			c.SkippedNotFound(test, e.Path)
		}
		err = nil
		return
	}
	defer r.Close()
	d := make(chan any, dataChanBufSize)
	go func() {
		var e error
		defer func() {
			if e != nil && e != io.EOF {
				d <- e
			}
			defer close(d)
		}()
		dc := gob.NewDecoder(r)
		var a any
		for {
			if e = dc.Decode(&a); e != nil {
				return
			}
			d <- a
		}
	}()
	err = rst.tee(x, d, test)
	return
}

// VetCommand loads and checks the CUE config.
type VetCommand struct {
}

// run implements command
func (*VetCommand) run(context.Context) (err error) {
	_, err = LoadConfig(&load.Config{})
	return
}
