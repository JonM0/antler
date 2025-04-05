// SPDX-License-Identifier: GPL-3.0-or-later

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/heistp/antler/node/metric"
)

type SysProcMonitor struct {
	ProcFiles []string

	Interval metric.Duration

	Out string

	io sync.WaitGroup
}

// Run implements runner
func (p *SysProcMonitor) Run(ctx context.Context, arg runArg) (ofb Feedback, err error) {
	p.io.Add(1)
	go p.monitorLoop(ctx, arg.rec)

	var x cancelFunc = func() error {
		p.io.Wait()
		return nil
	}
	arg.cxl <- x

	return
}

func (p *SysProcMonitor) monitorLoop(ctx context.Context, rec *recorder) {
	defer p.io.Done()
	dc := ctx.Done()
	procdata := make([][]string, len(p.ProcFiles))
	timedata := make([]time.Time, 0)

	rec.Logf("monitoring %d", len(p.ProcFiles))
	for {
		select {
		case <-dc:
			rec.Log("exiting")
			data := make(map[string]any)
			data["time"] = timedata
			for i, proc := range p.ProcFiles {
				data[proc] = procdata[i]
			}
			filedata, err := json.Marshal(data)
			if err != nil {
				rec.Logf("%s", err)
				return
			}
			rec.FileData(p.Out, filedata)
			return

		case <-time.After(p.Interval.Duration()):
			timedata = append(timedata, time.Now())
			for i, proc := range p.ProcFiles {
				d, err := p.checkProc(proc)
				if err != nil {
					rec.Logf("%s", err)
					continue
				}
				procdata[i] = append(procdata[i], string(d))
			}
		}
	}
}

func (p *SysProcMonitor) checkProc(proc string) (out []byte, err error) {
	var fi *os.File
	if fi, err = os.Open(proc); err != nil {
		err = fmt.Errorf("open %s: %w", proc, err)
		return
	}
	defer fi.Close()

	buf := make([]byte, 4096)
	for {
		var n int
		n, err = fi.Read(buf)
		if err != nil || n == 0 {
			if err == io.EOF {
				err = nil
			}
			return
		}
		out = append(out, buf[:n]...)
	}
}

type datapoint struct {
	Time time.Time
	Data []string
}

type procOutput struct {
	Procs  []string
	Points []datapoint
}
