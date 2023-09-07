// SPDX-License-Identifier: GPL-3.0
// Copyright 2023 Pete Heist

package antler

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Results configures the behavior for reading and writing result files, which
// include all output files and reports.
type Results struct {
	RootDir         string
	WorkDir         string
	ResultDirUTC    bool
	ResultDirFormat string
}

// open ensures the Results are ready for use. It must be called before other
// Results methods are used.
func (r Results) open() error {
	var e error
	if _, e = os.Stat(r.WorkDir); e == nil {
		return fmt.Errorf(
			"'%s' already exists- ensure no other test is running, then move it away",
			r.WorkDir)
	}
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	return e
}

// close finalizes the Results, and must be called after all results are
// written. Use of a defer statement is strongly advised.
func (r Results) close() (err error) {
	t := time.Now()
	if r.ResultDirUTC {
		t = t.UTC()
	}
	p := filepath.Join(r.RootDir, t.Format(r.ResultDirFormat))
	if err = os.Rename(r.WorkDir, p); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return
}

// root returns a resultRW with RootDir as the prefix.
func (r Results) root() resultRW {
	return resultRW{r.RootDir + string(os.PathSeparator)}
}

// work returns a resultRW with WorkDir as the prefix.
func (r Results) work() resultRW {
	return resultRW{r.WorkDir + string(os.PathSeparator)}
}

// resultInfo returns a list of ResultInfos by reading the directory names under
// RootDir that match ResultDirFormat. The returned ResultInfos are sorted
// descending by Name.
func (r Results) resultInfo() (ii []ResultInfo, err error) {
	var d *os.File
	if d, err = os.Open(r.RootDir); err != nil {
		return
	}
	defer d.Close()
	var ee []fs.DirEntry
	if ee, err = d.ReadDir(0); err != nil {
		return
	}
	for _, e := range ee {
		var i fs.FileInfo
		if i, err = e.Info(); err != nil {
			return
		}
		n := i.Name()
		if _, te := time.Parse(r.ResultDirFormat, n); te == nil {
			ii = append(ii, ResultInfo{n, filepath.Join(r.RootDir, n)})
		}
	}
	sort.Slice(ii, func(i, j int) bool {
		return ii[i].Name > ii[j].Name
	})
	return
}

// ResultInfo contains information on one result.
type ResultInfo struct {
	Name string // base name of result directory
	Path string // path to result directory
}

// resultRW provides a rwer implementation for a given path prefix.
type resultRW struct {
	prefix string
}

// Append returns a new resultRW by appending the given prefix to the prefix of
// this resultRW.
func (r resultRW) Append(prefix string) resultRW {
	return resultRW{r.prefix + prefix}
}

// Reader implements rwer
func (r resultRW) Reader(name string) (io.ReadCloser, error) {
	return os.Open(r.path(name))
}

// Writer implements rwer
func (r resultRW) Writer(name string) (wc io.WriteCloser, err error) {
	if name == "-" {
		wc = &stdoutWriter{}
		return
	}
	p := r.path(name)
	if d := filepath.Dir(p); d != string(os.PathSeparator) &&
		d != "." && d != ".." {
		if err = os.MkdirAll(d, 0755); err != nil {
			return
		}
	}
	wc, err = openAtomic(p)
	return
}

// atomicWriter is a WriteCloser for a given named file that first writes to a
// temporary file name~, then moves name~ to name when Close is called. Close
// *must* be called, so it's strongly suggested to call it in a defer, and check
// for any errors it may return.
//
// For safety, the Close method does *not* replace the named file if an error
// occurred during Write.
//
// atomicWriter is not safe for concurrent use.
type atomicWriter struct {
	name string
	tmp  *os.File
	err  bool
}

// openAtomic returns a new atomicWriter, open and ready for use.
func openAtomic(name string) (w *atomicWriter, err error) {
	w = &atomicWriter{name: name}
	w.tmp, err = os.Create(w.tmpName())
	return
}

// tmpName returns the name of the temporary file for writing.
func (a *atomicWriter) tmpName() string {
	return a.name + "~"
}

// Write implements io.Writer.
func (a *atomicWriter) Write(p []byte) (n int, err error) {
	if n, err = a.tmp.Write(p); err != nil {
		a.err = true
	}
	return
}

// Close implements io.Closer.
func (a *atomicWriter) Close() (err error) {
	if err = a.tmp.Close(); err != nil {
		return
	}
	if a.err {
		return
	}
	err = os.Rename(a.tmpName(), a.name)
	return
}

// path returns the path to a results file given its name.
func (r resultRW) path(name string) string {
	return filepath.Clean(r.prefix + name)
}

// readerer wraps the Reader method, to return a ReadCloser for reading results.
// The name parameter identifies the result data according to the underlying
// implementation, and is typically a filename, or filename suffix.
type readerer interface {
	Reader(name string) (io.ReadCloser, error)
}

// writerer wraps the Writer method, to return a WriteCloser for writing
// results. The name parameter identifies the result data according to the
// underlying implementation, and is typically a filename, or filename suffix.
type writerer interface {
	Writer(name string) (io.WriteCloser, error)
}

// rwer groups the readerer and writerer interfaces.
type rwer interface {
	readerer
	writerer
}

// stdoutWriter is a WriteCloser that writes to stdout. The Close implementation
// does nothing.
type stdoutWriter struct {
}

// Write implements io.Writer
func (stdoutWriter) Write(p []byte) (n int, err error) {
	return os.Stdout.Write(p)
}

// Close implements io.Closer
func (stdoutWriter) Close() error {
	return nil
}
