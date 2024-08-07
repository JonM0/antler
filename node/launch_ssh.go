// SPDX-License-Identifier: GPL-3.0
// Copyright 2022 Pete Heist

package node

import (
	"bufio"
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

//go:embed launch_ssh.tmpl
var sshTemplate string

// sshArgs contains the arguments passed to launch_ssh.tmpl.
type sshArgs struct {
	NodeID  string // node ID
	ExeName string // base name of the node executable
	ExeSize int64  // size of the node executable
}

// SSH is a launcher used to start an Antler node remotely via ssh.
type SSH struct {
	Destination string // ssh destination (man ssh(1))
}

// launch implements launcher
func (s *SSH) launch(node Node, log logFunc) (tr transport, err error) {
	if !node.Netns.zero() {
		err = fmt.Errorf("Netns not supported with the SSH launcher")
		return
	}
	if node.Env.varsSet() {
		err = fmt.Errorf("Env not supported with the SSH launcher")
		return
	}
	var script string
	if script, err = executeSSHTemplate(node); err != nil {
		return
	}
	var scmd string
	if scmd, err = scriptToCommand(script); err != nil {
		return
	}
	var r io.Reader
	if r, err = repo.Reader(node.Platform); err != nil {
		return
	}
	dest := s.Destination
	if dest == "" {
		dest = string(node.ID)
	}
	c := exec.Command("ssh", "-o", "BatchMode yes", dest,
		"sh", "-c", fmt.Sprintf("'%s'", scmd))
	c.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	log("%s", c)
	var nc *nodeCmd
	if nc, err = newNodeCmd(c, nil, log); err != nil {
		return
	}
	if err = nc.Start(); err != nil {
		return
	}
	if _, err = io.Copy(nc, r); err != nil {
		return
	}
	tr = newGobTransport(nc)
	return
}

// executeSSHTemplate runs the ssh template and returns the output as a string.
func executeSSHTemplate(node Node) (s string, err error) {
	t := template.New("launch_ssh").Funcs(template.FuncMap{
		"Platform": func(substr string) bool {
			return strings.Contains(node.Platform, substr)
		},
	})
	if t, err = t.Parse(sshTemplate); err != nil {
		return
	}
	var z int64
	if z, err = repo.Size(node.Platform); err != nil {
		return
	}
	data := sshArgs{
		string(node.ID),
		PlatformExeName(node.Platform).String(),
		z,
	}
	var b strings.Builder
	if err = t.Execute(&b, data); err != nil {
		return
	}
	s = b.String()
	return
}

// scriptToCommand converts the multi-line launch script generated by the
// template into a one line command. This is passed to sh -c, which is in turn
// passed to ssh.
func scriptToCommand(script string) (cmd string, err error) {
	in := bufio.NewScanner(bytes.NewBufferString(script))
	var out strings.Builder
	first := true
	var sep string
	comment := regexp.MustCompile("^\\#")
	unterminated := regexp.MustCompile("[\\s;](\\{|then|do)\\s*$")
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" || comment.MatchString(line) {
			continue
		}
		if !first {
			out.WriteString(sep)
		} else {
			first = false
		}
		out.WriteString(line)
		if unterminated.MatchString(line) {
			sep = " "
		} else {
			sep = "; "
		}
	}
	cmd = out.String()
	return
}
