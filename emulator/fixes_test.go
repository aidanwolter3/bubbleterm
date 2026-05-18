package emulator

import (
	"bytes"
	"os/exec"
	"testing"
	"time"
)

// TestVTResponseLoopNoDeadlock starts a program that sends the CSI c
// device-attributes query and asserts the process exits within 2 seconds.
// Without vtResponseLoop the vt emulator's internal io.Pipe blocks
// ptyReadLoop (which holds e.mu) and the emulator deadlocks.
func TestVTResponseLoopNoDeadlock(t *testing.T) {
	e, err := New(80, 24)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer e.Close()

	done := make(chan struct{}, 1)
	e.SetOnExit(func(_ string) {
		done <- struct{}{}
	})

	cmd := exec.Command("/bin/bash", "-c", `printf '\033[c'; sleep 0.05`)
	if err := e.StartCommand(cmd); err != nil {
		t.Fatalf("StartCommand: %v", err)
	}

	select {
	case <-done:
		// process exited — no deadlock
	case <-time.After(2 * time.Second):
		t.Fatal("vtResponseLoop deadlock: process did not exit within 2s")
	}
}

// TestSanitizeOSCC1 verifies that C1-range bytes inside OSC strings are
// replaced with '?' while bytes outside OSC strings are left unchanged.
func TestSanitizeOSCC1(t *testing.T) {
	e := &Emulator{}

	// ✳ is U+2733 (UTF-8: e2 9c b3). When inside an OSC window-title sequence
	// the 0x9C continuation byte is in the C1 range and would be misinterpreted
	// as C1 STRING TERMINATOR, prematurely dispatching the OSC and leaking the
	// remaining title text as visible cell content.
	in := []byte("\x1b]0;\xe2\x9c\xb3 Claude Code\x07")
	got := e.sanitizeOSCC1(in)
	want := []byte("\x1b]0;\xe2?\xb3 Claude Code\x07")
	if !bytes.Equal(got, want) {
		t.Errorf("sanitizeOSCC1 inside OSC:\ngot  %x\nwant %x", got, want)
	}

	// Bytes outside OSC strings must not be modified.
	e2 := &Emulator{}
	in2 := []byte("normal\x9ctext")
	got2 := e2.sanitizeOSCC1(in2)
	if !bytes.Equal(got2, in2) {
		t.Errorf("sanitizeOSCC1 modified bytes outside OSC: %x != %x", got2, in2)
	}
}
