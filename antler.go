// SPDX-License-Identifier: GPL-3.0
// Copyright 2022 Pete Heist

// Package antler contains types for running the Antler application.

package antler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"

	"cuelang.org/go/cue/load"
	"github.com/heistp/antler/node"
)

// dataChanBufLen is used as the buffer length for data channels.
const dataChanBufLen = 64

// Run runs an Antler Command.
func Run(ctx context.Context, cmd Command) error {
	return cmd.run(ctx)
}

// A Command is an Antler command.
type Command interface {
	run(context.Context) error
}

// VetCommand loads and checks the CUE config.
type VetCommand struct {
}

// run implements command
func (*VetCommand) run(context.Context) (err error) {
	_, err = LoadConfig(&load.Config{})
	return
}

// RunCommand runs tests and reports.
type RunCommand struct {
	// Filter selects which tests to run.
	Filter TestFilter

	// SkippedFiltered is called when a test was skipped because it was rejected
	// by the Filter.
	SkippedFiltered func(test *Test)
}

// run implements command
func (r RunCommand) run(ctx context.Context) (err error) {
	var c *Config
	if c, err = LoadConfig(&load.Config{}); err != nil {
		return
	}
	if err = c.Results.open(); err != nil {
		return
	}
	defer func() {
		if e := c.Results.close(); e != nil && err == nil {
			err = e
		}
	}()
	d := doRun{r, c.Results}
	err = c.Run.do(ctx, d, reportStack{})
	return
}

// doRun is a doer that runs a Test and its reports.
type doRun struct {
	RunCommand
	Results
}

// do implements doer
func (u doRun) do(ctx context.Context, test *Test, rst reportStack) (
	err error) {
	if u.Filter != nil && !u.Filter.Accept(test) {
		if u.SkippedFiltered != nil {
			u.SkippedFiltered(test)
		}
		return
	}
	var w io.WriteCloser
	if w, err = test.DataWriter(u.Results); err != nil {
		if _, ok := err.(NoDataFileError); !ok {
			return
		}
		err = nil
	}
	var a appendData
	p := test.During.report()
	if w != nil {
		p = append(p, writeData{w})
	} else {
		p = append(p, &a)
	}
	d := make(chan any, dataChanBufLen)
	ctx, x := context.WithCancelCause(ctx)
	defer x(nil)
	go node.Do(ctx, &test.Run, &exeSource{}, d)
	for e := range p.pipeline(ctx, d, nil, test.WorkRW(u.Results)) {
		x(e)
		if err == nil {
			err = e
		}
	}
	if err != nil {
		return
	}
	var s reporter
	if w != nil {
		var r io.ReadCloser
		if r, err = test.DataReader(u.Results); err != nil {
			return
		}
		s = readData{r}
	} else {
		s = rangeData(a)
	}
	err = teeReport(ctx, s, test, test.WorkRW(u.Results), rst)
	return
}

// teeReport runs the Test.Report and reportStack pipelines concurrently, using
// src to supply the data.
func teeReport(ctx context.Context, src reporter, test *Test, rw rwer,
	rst reportStack) (err error) {
	var r []report
	r = append(r, test.Report.report())
	r = append(r, rst.report())
	ctx, x := context.WithCancelCause(ctx)
	defer x(nil)
	for e := range report([]reporter{src}).tee(ctx, rw, nil, r...) {
		x(e)
		if err == nil {
			err = e
		}
	}
	return
}

// ReportCommand runs the After reports using the data files as the source.
type ReportCommand struct {
	// Filter selects which tests to run.
	Filter TestFilter

	// SkippedFiltered is called when a report was skipped because it was
	// rejected by the Filter.
	SkippedFiltered func(test *Test)

	// SkippedNoDataFile is called when a report was skipped because the Test's
	// DataFile field is empty.
	SkippedNoDataFile func(test *Test)

	// SkippedNotFound is called when a report was skipped because the data file
	// needed to run it doesn't exist.
	SkippedNotFound func(test *Test, path string)
}

// run implements command
func (r ReportCommand) run(ctx context.Context) (err error) {
	var c *Config
	if c, err = LoadConfig(&load.Config{}); err != nil {
		return
	}
	if err = r.workaround(&c.Results); err != nil {
		return
	}
	if err = c.Results.open(); err != nil {
		return
	}
	defer func() {
		if e := c.Results.close(); e != nil && err == nil {
			err = e
		}
	}()
	d := doReport{r, c.Results}
	err = c.Run.do(ctx, d, reportStack{})
	return
}

// workaround makes the report command work prior to the implementation of
// incremental test runs by changing WorkDir to point to the latest result
// directory, and forcing Destructive mode to true.
//
// TODO remove report workaround after incremental runs are working
func (r ReportCommand) workaround(res *Results) (err error) {
	var ii []ResultInfo
	if ii, err = res.resultInfo(); err != nil {
		return
	}
	if len(ii) == 0 {
		err = fmt.Errorf("no results found in '%s'", res.RootDir)
		return
	}
	res.WorkDir = ii[0].Path
	res.Destructive = true
	return
}

// doReport is a doer that runs reports.
type doReport struct {
	ReportCommand
	Results
}

// do implements doer
func (d doReport) do(ctx context.Context, test *Test, rst reportStack) (
	err error) {
	if d.Filter != nil && !d.Filter.Accept(test) {
		if d.SkippedFiltered != nil {
			d.SkippedFiltered(test)
		}
		return
	}
	var r io.ReadCloser
	if r, err = test.DataReader(d.Results); err != nil {
		if _, ok := err.(NoDataFileError); ok {
			if d.SkippedNoDataFile != nil {
				d.SkippedNoDataFile(test)
			}
			err = nil
			return
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return
		}
		e := err.(*fs.PathError)
		if d.SkippedNotFound != nil {
			d.SkippedNotFound(test, e.Path)
		}
		err = nil
		return
	}
	err = teeReport(ctx, readData{r}, test, test.WorkRW(d.Results), rst)
	return
}

// ServerCommand runs the builtin web server.
type ServerCommand struct {
}

// run implements command
func (s ServerCommand) run(ctx context.Context) (err error) {
	var c *Config
	if c, err = LoadConfig(&load.Config{}); err != nil {
		return
	}
	log.SetPrefix("")
	log.SetFlags(0)
	log.SetOutput(os.Stdout)
	err = c.Server.Run()
	return
}
