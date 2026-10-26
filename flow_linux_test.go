//
// Copyright 2014-2026 Cristian Maglie. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

package serial

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ptyPair starts socat with two linked pseudo terminals and returns the path of the first one.
// The test is skipped when socat is not installed.
func ptyPair(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat is not installed")
	}
	cmd := exec.Command("socat", "-d", "-d", "pty,raw,echo=0", "pty,raw,echo=0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	var out []byte
	buf := make([]byte, 1024)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := stderr.Read(buf)
		out = append(out, buf[:n]...)
		for _, l := range strings.Split(string(out), "\n") {
			if i := strings.Index(l, "PTY is "); i >= 0 {
				return strings.TrimSpace(l[i+len("PTY is "):])
			}
		}
	}
	t.Fatalf("socat did not report a pty: %q", out)
	return ""
}

// The library opens the port exclusively, so the settings are read through a descriptor that
// was opened before it.
func TestFlowControlSetsTheTermiosFlags(t *testing.T) {
	name := ptyPair(t)
	fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	flags := func() (rtscts, ixon, ixoff bool) {
		tt, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		return tt.Cflag&unix.CRTSCTS != 0, tt.Iflag&unix.IXON != 0, tt.Iflag&unix.IXOFF != 0
	}
	cases := []struct {
		name                string
		mode                FlowControl
		rtscts, ixon, ixoff bool
	}{
		{"zero value is none", 0, false, false, false},
		{"rts/cts", RTSCTSFlowControl, true, false, false},
		{"xon/xoff", XONXOFFFlowControl, false, true, true},
		{"none again", NoFlowControl, false, false, false},
	}
	p, err := Open(name, &Mode{BaudRate: 115200, FlowControl: NoFlowControl})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	// Open applies the mode it is given; SetMode must change it afterwards.
	for _, c := range cases {
		if err := p.SetMode(&Mode{BaudRate: 115200, FlowControl: c.mode}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if a, b, d := flags(); a != c.rtscts || b != c.ixon || d != c.ixoff {
			t.Errorf("%s: CRTSCTS=%v IXON=%v IXOFF=%v, want %v %v %v", c.name, a, b, d, c.rtscts, c.ixon, c.ixoff)
		}
	}
}

func TestOpenAppliesFlowControl(t *testing.T) {
	for _, c := range []struct {
		mode   FlowControl
		rtscts bool
		xon    bool
	}{{NoFlowControl, false, false}, {RTSCTSFlowControl, true, false}, {XONXOFFFlowControl, false, true}} {
		name := ptyPair(t)
		fd, err := unix.Open(name, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		p, err := Open(name, &Mode{BaudRate: 9600, FlowControl: c.mode})
		if err != nil {
			unix.Close(fd)
			t.Fatal(err)
		}
		tt, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		if got := tt.Cflag&unix.CRTSCTS != 0; got != c.rtscts {
			t.Errorf("mode %d: CRTSCTS=%v, want %v", c.mode, got, c.rtscts)
		}
		if got := tt.Iflag&unix.IXON != 0 && tt.Iflag&unix.IXOFF != 0; got != c.xon {
			t.Errorf("mode %d: IXON and IXOFF=%v, want %v", c.mode, got, c.xon)
		}
		p.Close()
		unix.Close(fd)
	}
}
