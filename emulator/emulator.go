package emulator

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
	"github.com/google/uuid"
)

// oscParseState is a minimal parser state used by sanitizeOSCC1 to track
// whether the byte stream is currently inside an OSC string sequence.
type oscParseState uint8

const (
	oscGround   oscParseState = iota // normal ground state
	oscEsc                           // just saw 0x1B
	oscInString                      // inside an OSC (or DCS/SOS/APC/PM) string
	oscInStrEsc                      // saw 0x1B inside a string (potential 7-bit ST)
)

// Emulator is a headless terminal emulator that maintains internal state
// and renders to a framebuffer instead of directly to screen
type Emulator struct {
	mu sync.RWMutex
	id string

	// oscState tracks whether we are inside an OSC/DCS/SOS/APC/PM string so
	// that sanitizeOSCC1 can replace C1-range bytes (0x80–0x9F) that appear
	// as UTF-8 continuation bytes and would otherwise be misinterpreted as C1
	// control codes (e.g. 0x9C as STRING TERMINATOR) by the x/ansi parser.
	oscState oscParseState

	// Terminal emulator (using charm's x/vt)
	vt *vt.Emulator

	// PTY for process communication
	pty, tty *os.File

	// Pipe-based I/O (alternative to PTY)
	reader io.Reader
	writer io.WriteCloser
	isPipe bool

	// Process tracking
	cmd           *exec.Cmd
	processExited bool
	onExit        func(string, error) // called when process exits: id, exit error

	// Framerate control
	frameRate time.Duration
	stopChan  chan struct{}

	// Damage tracking for change detection
	lastRender string
	damaged    bool

	// Screen dimensions
	width, height int
}

// EmittedFrame represents a rendered frame from the terminal.
type EmittedFrame struct {
	Rows   []string     // Each row is a string with ANSI escape codes embedded
	Damage []LineDamage // Lines that changed since the last GetScreen call
}

// New creates a new headless terminal emulator
func New(cols, rows int) (*Emulator, error) {
	e := &Emulator{
		vt:        vt.NewEmulator(cols, rows),
		id:        uuid.New().String(),
		frameRate: time.Second / 30, // Default 30 FPS
		stopChan:  make(chan struct{}),
		width:     cols,
		height:    rows,
		damaged:   true, // Initial render needed
	}

	var err error
	e.pty, e.tty, err = pty.Open()
	if err != nil {
		return nil, err
	}

	// Set initial size
	err = e.resize(cols, rows)
	if err != nil {
		return nil, err
	}

	go e.ptyReadLoop()
	go e.vtResponseLoop()

	return e, nil
}

// NewFromPipes creates a headless terminal emulator that reads output from r
// and writes input to w, instead of using a PTY. This is useful when the
// process is already running and you have access to its stdin/stdout pipes.
// The caller is responsible for closing the reader when the process exits.
func NewFromPipes(cols, rows int, r io.Reader, w io.WriteCloser) (*Emulator, error) {
	e := &Emulator{
		vt:        vt.NewEmulator(cols, rows),
		id:        uuid.New().String(),
		frameRate: time.Second / 30,
		stopChan:  make(chan struct{}),
		reader:    r,
		writer:    w,
		isPipe:    true,
		width:     cols,
		height:    rows,
		damaged:   true,
	}

	go e.ptyReadLoop()
	go e.vtResponseLoop()

	return e, nil
}

func (e *Emulator) ID() string {
	return e.id
}

// SetSize sets the terminal size (same as Resize for now)
func (e *Emulator) SetSize(cols, rows int) error {
	return e.Resize(cols, rows)
}

// Resize changes the terminal dimensions
func (e *Emulator) Resize(cols, rows int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resize(cols, rows)
}

func (e *Emulator) resize(cols, rows int) error {
	if !e.isPipe {
		err := pty.Setsize(e.pty, &pty.Winsize{
			Rows: uint16(rows),
			Cols: uint16(cols),
			X:    uint16(cols * 8),
			Y:    uint16(rows * 16),
		})
		if err != nil {
			return err
		}
	}

	e.vt.Resize(cols, rows)
	e.width = cols
	e.height = rows
	e.damaged = true

	return nil
}

// SetFrameRate sets the internal render loop framerate
func (e *Emulator) SetFrameRate(fps int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.frameRate = time.Second / time.Duration(fps)
}

// GetScreen returns the current rendered screen as ANSI strings.
// It also returns damage information about which lines changed since
// the last call.
func (e *Emulator) GetScreen() EmittedFrame {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Render the current screen
	rendered := e.vt.Render()

	// Check for changes
	var damage []LineDamage
	if rendered != e.lastRender || e.damaged {
		// Mark all lines as damaged for simplicity
		// (the vt package tracks touched lines but we simplify here)
		for y := 0; y < e.height; y++ {
			damage = append(damage, LineDamage{
				Row:    y,
				X1:     0,
				X2:     e.width,
				Reason: CRText,
			})
		}
		e.lastRender = rendered
		e.damaged = false
	}

	// Split rendered output into rows
	rows := splitIntoRows(rendered, e.height, e.width)

	return EmittedFrame{Rows: rows, Damage: damage}
}

// splitIntoRows splits the rendered output into individual rows and pads to width
func splitIntoRows(rendered string, height, width int) []string {
	rows := make([]string, height)

	// The vt.Render() returns a string with ANSI codes
	// We need to split it by newlines while preserving ANSI codes
	currentRow := 0
	var currentLine string

	for _, r := range rendered {
		if r == '\n' {
			if currentRow < height {
				rows[currentRow] = padRow(currentLine, width)
				currentRow++
			}
			currentLine = ""
		} else {
			currentLine += string(r)
		}
	}

	// Handle last line if no trailing newline
	if currentRow < height && currentLine != "" {
		rows[currentRow] = padRow(currentLine, width)
		currentRow++
	}

	// Fill remaining rows with spaces
	emptyRow := strings.Repeat(" ", width)
	for i := currentRow; i < height; i++ {
		if rows[i] == "" {
			rows[i] = emptyRow
		}
	}

	return rows
}

// padRow pads a row to the specified width, accounting for ANSI escape codes.
// It always appends a SGR reset (\033[0m) before any trailing spaces so that
// active attributes (e.g. underline, bold) from the row's content do not bleed
// into the padding or into subsequent rows when rows are joined with \n.
func padRow(row string, width int) string {
	const reset = "\033[0m"
	if visibleLen := ansi.StringWidth(row); visibleLen < width {
		return row + reset + strings.Repeat(" ", width-visibleLen)
	}
	return row + reset
}

// Cursor returns the current cursor position and whether the cursor is visible.
func (e *Emulator) Cursor() (Pos, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	pos := e.vt.CursorPosition()
	// The vt package doesn't expose cursor visibility directly in a simple way
	// Default to visible
	return Pos{X: pos.X, Y: pos.Y}, true
}

// SetOnExit registers a callback invoked when the process exits.
// The callback receives the emulator ID and the process exit error (nil on clean exit).
func (e *Emulator) SetOnExit(cb func(id string, exitErr error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.onExit = cb
}

// IsProcessExited returns true if the process has exited
func (e *Emulator) IsProcessExited() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.processExited
}

// StartCommand starts a command in the terminal.
// This is not supported for pipe-based emulators; use NewFromPipes instead.
func (e *Emulator) StartCommand(cmd *exec.Cmd) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.isPipe {
		return fmt.Errorf("StartCommand is not supported on pipe-based emulators")
	}

	if e.pty == nil {
		return ErrPTYNotInitialized
	}

	// Set up environment
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}

	// Ensure TERM is set correctly
	termSet := false
	for i, env := range cmd.Env {
		if len(env) >= 5 && env[:5] == "TERM=" {
			cmd.Env[i] = "TERM=xterm-256color"
			termSet = true
			break
		}
	}
	if !termSet {
		cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	}

	// Connect to PTY
	cmd.Stdout = e.tty
	cmd.Stdin = e.tty
	cmd.Stderr = e.tty

	// Set up process group for proper signal handling
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Setsid = true
	// Don't set Ctty explicitly - let the system handle it

	// Store the command reference
	e.cmd = cmd
	e.processExited = false

	err := cmd.Start()
	if err != nil {
		return err
	}

	// Start monitoring the process in a goroutine
	go e.monitorProcess()

	return nil
}

// monitorProcess waits for the process to exit and calls the exit callback.
func (e *Emulator) monitorProcess() {
	if e.cmd == nil {
		return
	}

	exitErr := e.cmd.Wait()

	// Close tty before acquiring the mutex to unblock ptyReadLoop promptly on
	// macOS. The master PTY side does not return EIO until all slave file
	// descriptors are closed; without this, ptyReadLoop blocks indefinitely
	// even after the process has exited.
	if e.tty != nil {
		e.tty.Close()
	}

	e.mu.Lock()
	e.processExited = true
	onExit := e.onExit
	id := e.id
	e.mu.Unlock()

	if onExit != nil {
		onExit(id, exitErr)
	}
}

// Write sends data to the PTY or pipe (keyboard input)
func (e *Emulator) Write(data []byte) (int, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if e.isPipe {
		if e.writer == nil {
			return 0, ErrPTYNotInitialized
		}
		return e.writer.Write(data)
	}

	if e.pty == nil {
		return 0, ErrPTYNotInitialized
	}

	return e.pty.Write(data)
}

// SendKey sends a key event to the terminal
func (e *Emulator) SendKey(key string) error {
	_, err := e.Write([]byte(key))
	return err
}

// SendMouse sends a mouse event to the terminal in SGR format
func (e *Emulator) SendMouse(button int, x, y int, pressed bool) error {
	// Convert to the vt package's mouse event format
	var vtButton vt.MouseButton
	switch button {
	case 0:
		vtButton = vt.MouseLeft
	case 1:
		vtButton = vt.MouseMiddle
	case 2:
		vtButton = vt.MouseRight
	case -1:
		vtButton = vt.MouseNone // Motion
	default:
		vtButton = vt.MouseButton(button)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if pressed {
		e.vt.SendMouse(vt.MouseClick{
			Button: vtButton,
			X:      x,
			Y:      y,
		})
	} else if button == -1 {
		e.vt.SendMouse(vt.MouseMotion{
			Button: vtButton,
			X:      x,
			Y:      y,
		})
	} else {
		e.vt.SendMouse(vt.MouseRelease{
			Button: vtButton,
			X:      x,
			Y:      y,
		})
	}

	return nil
}

// Close shuts down the emulator
func (e *Emulator) Close() error {
	close(e.stopChan)

	if e.isPipe {
		if e.writer != nil {
			e.writer.Close()
		}
		return nil
	}

	if e.tty != nil {
		e.tty.Close()
	}
	if e.pty != nil {
		e.pty.Close()
	}

	return e.vt.Close()
}

// FeedBytes feeds raw bytes directly into the VT emulator (bypassing PTY).
// Useful for one-shot log replay without starting a process.
func (e *Emulator) FeedBytes(data []byte) {
	clean := e.sanitizeOSCC1(data)
	e.mu.Lock()
	e.vt.Write(clean)
	e.damaged = true
	e.mu.Unlock()
}

// sanitizeOSCC1 replaces C1 control bytes (0x80–0x9F) inside OSC/DCS/SOS/APC/PM
// string sequences with 0x3F ('?'). This prevents the x/ansi parser from
// treating a UTF-8 continuation byte such as 0x9C in ✳ (U+2733) as a C1
// STRING TERMINATOR, which would prematurely dispatch the OSC and leak the
// remainder of the title string as visible cell text.
func (e *Emulator) sanitizeOSCC1(in []byte) []byte {
	hasHigh := false
	for _, b := range in {
		if b >= 0x80 {
			hasHigh = true
			break
		}
	}
	if !hasHigh && e.oscState == oscGround {
		return in
	}

	out := make([]byte, 0, len(in))
	for _, b := range in {
		switch e.oscState {
		case oscGround:
			if b == 0x1B {
				e.oscState = oscEsc
			}
			out = append(out, b)

		case oscEsc:
			if b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_' {
				e.oscState = oscInString
			} else {
				e.oscState = oscGround
			}
			out = append(out, b)

		case oscInString:
			switch {
			case b == 0x07:
				e.oscState = oscGround
				out = append(out, b)
			case b == 0x1B:
				e.oscState = oscInStrEsc
				out = append(out, b)
			case b >= 0x80 && b <= 0x9F:
				out = append(out, '?')
			default:
				out = append(out, b)
			}

		case oscInStrEsc:
			if b == '\\' {
				e.oscState = oscGround
			} else {
				e.oscState = oscInString
			}
			out = append(out, b)
		}
	}
	return out
}

// vtResponseLoop reads terminal responses from the vt emulator's internal pipe
// (e.g. device-attribute replies to CSI c) and forwards them back to the child
// process. Without this goroutine the synchronous io.Pipe inside the vt emulator
// blocks the very first response write, which stalls ptyReadLoop while it holds
// e.mu and deadlocks the entire emulator.
func (e *Emulator) vtResponseLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := e.vt.Read(buf)
		if err != nil {
			return
		}
		if n > 0 {
			if e.isPipe {
				if e.writer != nil {
					e.writer.Write(buf[:n]) //nolint:errcheck
				}
			} else {
				if e.pty != nil {
					e.pty.Write(buf[:n]) //nolint:errcheck
				}
			}
		}
	}
}

// ptyReadLoop reads from PTY/pipe and writes to the vt emulator
func (e *Emulator) ptyReadLoop() {
	var source io.Reader
	if e.isPipe {
		source = e.reader
	} else {
		source = e.pty
	}

	buf := make([]byte, 4096)
	for {
		select {
		case <-e.stopChan:
			return
		default:
		}

		n, err := source.Read(buf)
		if err != nil {
			return
		}

		if n > 0 {
			clean := e.sanitizeOSCC1(buf[:n])
			e.mu.Lock()
			e.vt.Write(clean)
			e.damaged = true
			e.mu.Unlock()
		}
	}
}
