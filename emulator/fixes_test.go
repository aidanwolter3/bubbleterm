package emulator

import (
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

