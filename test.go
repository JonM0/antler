// SPDX-License-Identifier: GPL-3.0
// Copyright 2022 Pete Heist

package antler

import (
	"encoding/gob"
	"fmt"
	"html/template"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/heistp/antler/node"
)

// Test is an Antler test.
type Test struct {
	// ID uniquely identifies the Test in the test package.
	ID TestID

	// Group is the Group that this Test belongs to.
	Group *Group

	// Path is the path prefix for result files.
	Path string

	// ResultPrefix is the path prefix for result files. It may use Go template
	// syntax, and is further documented in config.cue.
	ResultPrefix string

	// DataFile is the name of the gob file containing the raw result data. If
	// empty, raw result data is not saved for the Test.
	DataFile string

	// Run is the top-level Run instance.
	node.Run

	// During is a pipeline of Reports run while the Test runs.
	During Report

	// After is a pipeline of Reports run after the Test completes.
	After Report

	// DuringDefault contains default reporters to be run while the Test is run.
	DuringDefault Report

	// ReportDefault contains default reporters to be run after the Test is run.
	ReportDefault Report

	// ResultPrefixX contains the output of the executed ResultPrefix template.
	ResultPrefixX string
}

// TestID represents a compound Test identifier. Keys and values must match the
// regex defined in config.cue.
type TestID map[string]string

// Equal returns true if other is equal to this TestID (they contain the same
// key/value pairs).
func (i TestID) Equal(other TestID) bool {
	return maps.Equal(i, other)
}

// String returns the Test ID in the form: [K=V ...] with key/value pairs
// sorted by their keys.
func (i TestID) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[")
	kk := make([]string, 0, len(i))
	for k := range i {
		kk = append(kk, k)
	}
	sort.Strings(kk)
	for j, k := range kk {
		if j > 0 {
			fmt.Fprintf(&b, " ")
		}
		fmt.Fprintf(&b, "%s=%s", k, i[k])
	}
	fmt.Fprintf(&b, "]")
	return b.String()
}

// DataWriter returns a WriteCloser for writing result data to the work
// directory.
//
// If DataFile is empty, DataFileUnsetError is returned.
func (t *Test) DataWriter(rw resultRW) (wc io.WriteCloser, err error) {
	if t.DataFile == "" {
		err = DataFileUnsetError{t}
		return
	}
	wc = rw.Writer(t.DataFile)
	return
}

// DataReader returns a ReadCloser for reading result data.
//
// If DataFile is empty, DataFileUnsetError is returned.
//
// If the data file does not exist, errors.Is(err, fs.ErrNotExist) returns true.
func (t *Test) DataReader(rw resultRW) (rc io.ReadCloser, err error) {
	if t.DataFile == "" {
		err = DataFileUnsetError{t}
		return
	}
	rc, err = rw.Reader(t.DataFile)
	return
}

// DataFileUnsetError is returned by DataWriter or DataReader when the Test's
// DataFile field is empty, so no data may be read or written. The Test field
// is the corresponding Test.
type DataFileUnsetError struct {
	Test *Test
}

// Error implements error
func (n DataFileUnsetError) Error() string {
	return fmt.Sprintf("DataFile field is empty for: '%s'\n", n.Test.ID)
}

// DataHasError returns true if the DataFile exists and has errors. See
// DataReader for the errors that may be returned.
func (t *Test) DataHasError(rw resultRW) (hasError bool, err error) {
	var r io.ReadCloser
	if r, err = t.DataReader(rw); err != nil {
		return
	}
	defer func() {
		if e := r.Close(); e != nil && err == nil {
			err = e
		}
	}()
	c := gob.NewDecoder(r)
	for {
		var a any
		if err = c.Decode(&a); err != nil {
			if err == io.EOF {
				err = nil
			}
			return
		}
		if _, ok := a.(error); ok {
			hasError = true
			return
		}
	}
}

// RW returns a child resultRW for reading and writing this Test's results.
func (t *Test) RW(work resultRW) resultRW {
	return work.Child(t.ResultPrefixX)
}

// LinkPriorData creates hard links to the most recent result data for this
// Test. DataFile is linked, along with any FileRefs it contains.
//
// If DataFile is empty, DataFileUnsetError is returned.
//
// If no prior result data for this Test could be found, LinkError is returned.
func (t *Test) LinkPriorData(rw resultRW) (err error) {
	if t.DataFile == "" {
		err = DataFileUnsetError{t}
		return
	}
	if err = rw.Link(t.DataFile); err != nil {
		return
	}
	var r io.ReadCloser
	if r, err = t.DataReader(rw); err != nil {
		return
	}
	defer func() {
		if e := r.Close(); e != nil && err == nil {
			err = e
		}
	}()
	c := gob.NewDecoder(r)
	for {
		var a any
		if err = c.Decode(&a); err != nil {
			if err == io.EOF {
				err = nil
				break
			}
			return
		}
		if l, k := a.(FileRef); k {
			if err = rw.Link(l.Name); err != nil {
				return
			}
		}
	}
	return
}

// Tests wraps a list of Tests to add functionality.
type Tests []Test

// validateTestIDs returns an error if any Test IDs are duplicated.
func (s Tests) validateTestIDs() (err error) {
	var ii, dd []TestID
	for _, t := range s {
		f := func(id TestID) bool {
			return id.Equal(t.ID)
		}
		if slices.ContainsFunc(ii, f) {
			if !slices.ContainsFunc(dd, f) {
				dd = append(dd, t.ID)
			}
		} else {
			ii = append(ii, t.ID)
		}
	}
	if len(dd) > 0 {
		err = DuplicateTestIDError3{dd}
		return
	}
	return
}

// DuplicateTestIDError3 is returned when multiple Tests have the same ID.
//
// TODO rename DuplicateTestIDError3 after Group removal
type DuplicateTestIDError3 struct {
	ID []TestID
}

// Error implements error
func (d DuplicateTestIDError3) Error() string {
	var s []string
	for _, i := range d.ID {
		s = append(s, i.String())
	}
	return fmt.Sprintf("duplicate Test IDs: %s", strings.Join(s, ", "))
}

// generatePaths expands any Path fields that use Go templates, and returns an
// error if any Paths are duplicated.
func (s Tests) generatePaths() (err error) {
	pp := make(map[string]int)
	var d []string
	for i := range s {
		t := &s[i]
		pt := template.New("Path")
		if pt, err = pt.Parse(t.Path); err != nil {
			return
		}
		var pb strings.Builder
		if err = pt.Execute(&pb, t.ID); err != nil {
			return
		}
		p := pb.String()
		t.Path = p
		// TODO set Test.Path only after Groups and TestRuns are removed
		t.ResultPrefixX = p
		if v, ok := pp[p]; ok {
			if v == 1 {
				d = append(d, p)
			}
			pp[p] = v + 1
		} else {
			pp[p] = 1
		}
	}
	if len(d) > 0 {
		err = DuplicatePathError{d}
	}
	return
}

// DuplicatePathError is returned when multiple Tests have the same Path.
type DuplicatePathError struct {
	Path []string
}

// Error implements error
func (d DuplicatePathError) Error() string {
	return fmt.Sprintf("duplicate Test Paths: %s", strings.Join(d.Path, ", "))
}
