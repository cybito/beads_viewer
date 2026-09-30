package ui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/list"
)

// imeReporter holds one lease for the lifetime of the interactive TUI. It never
// reads or changes an input source; the local ime-control service owns that state.
// The mutex also serializes shutdown signals with Bubble Tea's Update goroutine.
type imeReporter struct {
	mu              sync.Mutex
	path            string
	conn            net.Conn
	reader          *bufio.Reader
	state           string
	active          bool
	suspended       bool
	suspendInactive bool // blur already released this lease before editor handoff
	closed          bool
}

type imeRequest struct {
	Op     string `json:"op"`
	State  string `json:"state,omitempty"`
	Policy string `json:"policy,omitempty"`
}

type imeResponse struct {
	OK         bool   `json:"ok"`
	Generation uint64 `json:"generation"`
	Session    string `json:"session"`
	Error      string `json:"error"`
}

func newIMEReporter(path string) *imeReporter { return &imeReporter{path: path} }

func localIMESocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local/state/infra-as-code/ime-control/run/control.sock"), nil
}

// exchange waits for the daemon's durable-transition ACK before returning to
// keyboard dispatch. A failed transaction drops the lease rather than claiming
// that the requested mode was protected.
func (r *imeReporter) exchange(request imeRequest) error {
	if r.conn == nil {
		conn, err := net.DialTimeout("unix", r.path, 2*time.Second)
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		r.conn = conn
		r.reader = bufio.NewReaderSize(conn, 4096)
	}
	if err := r.conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		r.drop()
		return err
	}
	frame, err := json.Marshal(request)
	if err != nil {
		return err
	}
	frame = append(frame, '\n')
	if len(frame) > 4096 {
		return errors.New("IME request exceeds 4096 bytes")
	}
	n, err := r.conn.Write(frame)
	if err != nil || n != len(frame) {
		r.drop()
		if err == nil {
			err = errors.New("short IME request write")
		}
		return fmt.Errorf("send: %w", err)
	}
	line, err := r.reader.ReadSlice('\n')
	if err != nil || len(line) > 4096 {
		r.drop()
		if err == nil {
			err = errors.New("IME ACK exceeds 4096 bytes")
		}
		return fmt.Errorf("receive IME ACK: %w", err)
	}
	var response imeResponse
	if err := json.Unmarshal(line, &response); err != nil {
		r.drop()
		return fmt.Errorf("invalid IME ACK: %w", err)
	}
	if !response.OK {
		r.drop()
		return fmt.Errorf("IME service rejected %s: %s", request.Op, response.Error)
	}
	if response.Generation == 0 || response.Session == "" {
		r.drop()
		return errors.New("IME service returned incomplete ACK")
	}
	return nil
}

func (r *imeReporter) drop() {
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.conn = nil
	r.reader = nil
	r.active = false
}

func (r *imeReporter) report(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.suspended {
		return nil
	}
	if !r.active {
		if err := r.exchange(imeRequest{Op: "activate", State: state, Policy: "mode"}); err != nil {
			return err
		}
		r.active = true
	} else if r.state != state {
		if err := r.exchange(imeRequest{Op: "state", State: state}); err != nil {
			return err
		}
	} else {
		return nil
	}
	r.state = state
	return nil
}

func (r *imeReporter) blur() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active || r.closed || r.suspended {
		return nil
	}
	err := r.exchange(imeRequest{Op: "blur"})
	r.active = false
	return err
}

func (r *imeReporter) suspend() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.suspended {
		return nil
	}
	r.suspendInactive = !r.active
	r.suspended = true
	if r.active {
		if err := r.exchange(imeRequest{Op: "suspend"}); err != nil {
			r.suspended = false
			r.suspendInactive = false
			return err
		}
	}
	r.active = false
	return nil
}

func (r *imeReporter) resume(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.suspended {
		return nil
	}
	op := "resume"
	if r.conn == nil || r.suspendInactive {
		// A broken or already-blurred lease must activate a new owner.
		op = "activate"
	}
	request := imeRequest{Op: op, State: state}
	if op == "activate" {
		request.Policy = "mode"
	}
	if err := r.exchange(request); err != nil {
		r.suspended = false
		return err
	}
	r.state = state
	r.active = true
	r.suspended = false
	r.suspendInactive = false
	return nil
}

func (r *imeReporter) isSuspended() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.suspended
}

func (r *imeReporter) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var err error
	if r.conn != nil {
		err = r.exchange(imeRequest{Op: "close"})
	}
	r.drop()
	return err
}

// EnableTUIIME is called only by the interactive program path. Robot, version
// and debug-render paths never create or contact an IME client.
func (m *Model) EnableTUIIME() error {
	path, err := localIMESocket()
	if err != nil {
		return err
	}
	m.imeReporter = newIMEReporter(path)
	m.imeFocused = true
	// Obtain the command-mode ACK before entering Bubble Tea's input loop.
	return m.imeReporter.report(m.imeState())
}

// IMEFailure reports why the interactive TUI stopped without a protected mode.
func (m *Model) IMEFailure() error {
	if m.imeWarning != "" {
		return errors.New(m.imeWarning)
	}
	return nil
}

func (m *Model) imeState() string {
	if (m.focused == focusList && m.list.FilterState() == list.Filtering) ||
		(m.focused == focusBoard && m.board.IsSearchMode()) ||
		(m.focused == focusHistory && m.historyView.IsSearchActive()) ||
		(m.focused == focusLabelPicker && m.showLabelPicker && m.labelPicker.input.Focused()) ||
		(m.focused == focusTimeTravelInput && m.showTimeTravelPrompt && m.timeTravelInput.Focused()) {
		return "text"
	}
	return "command"
}

func (m *Model) noteIMEError(err error) {
	if err != nil {
		m.imeWarning = "IME mode unprotected: " + err.Error()
	} else {
		m.imeWarning = ""
	}
}

func (m *Model) reportIME() {
	if m.imeReporter != nil && m.imeFocused && !m.imeReporter.isSuspended() {
		m.noteIMEError(m.imeReporter.report(m.imeState()))
	}
}

// CloseIME releases the lease synchronously, including from signal shutdown.
func (m *Model) CloseIME() {
	if m.imeReporter != nil {
		if err := m.imeReporter.close(); err != nil {
			fmt.Fprintln(os.Stderr, "bv: IME close:", err)
		}
	}
}
