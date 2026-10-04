package ui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/list"
)

// imeReporter serializes one interactive app's lifecycle. Herdr receives only
// intent; ordinary local terminals use the daemon that owns the input source.
// A failed connection is never reopened with a cached mode.
type imeReporter struct {
	mu             sync.Mutex
	path           string
	openParams     map[string]string // nil for the ordinary local daemon
	conn           net.Conn
	reader         *bufio.Reader
	session        string
	generation     uint64
	state          string
	declared       bool
	active         bool // applied locally, or recorded by Herdr; never inferred
	suspended      bool // terminal is currently handed to a child
	resumeRequired bool
	closed         bool
	failure        error
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
	Scope      string `json:"scope"`
	Error      string `json:"error"`
}

type imeOpenResponse struct {
	ID     string `json:"id"`
	Result *struct {
		Type       string `json:"type"`
		Session    string `json:"session"`
		Generation uint64 `json:"generation"`
	} `json:"result"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newIMEReporter(path string) *imeReporter { return &imeReporter{path: path} }

func localIMESocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local/state/infra-as-code/ime-control/run/control.sock"), nil
}

func selectIMEReporter() (*imeReporter, error) {
	marker, present := os.LookupEnv("HERDR_IME_INTENT")
	if !present {
		path, err := localIMESocket()
		if err != nil {
			return nil, err
		}
		return newIMEReporter(path), nil
	}
	if marker != "1" {
		return nil, errors.New("HERDR_IME_INTENT_UNSUPPORTED: expected marker 1")
	}
	if os.Getenv("HERDR_ENV") != "1" {
		return nil, errors.New("HERDR_IME_INTENT_INVALID: HERDR_ENV must be 1")
	}
	pane, popup := os.Getenv("HERDR_PANE_ID"), os.Getenv("HERDR_IME_POPUP_TERMINAL_ID")
	if (pane == "") == (popup == "") {
		return nil, errors.New("HERDR_IME_INTENT_INVALID: exactly one pane or popup identity is required")
	}
	path := os.Getenv("HERDR_SOCKET_PATH")
	field, identity := "pane_id", pane
	if popup != "" {
		field, identity = "popup_terminal_id", popup
	}
	params := map[string]string{field: identity}
	return &imeReporter{path: path, openParams: params}, nil
}

func decodeIMEFrame(frame []byte, response any) error {
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(response); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("multiple JSON values in IME ACK")
	}
	return nil
}

func (r *imeReporter) transact(request any, response any) error {
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
		if err == nil {
			err = errors.New("short IME request write")
		}
		return fmt.Errorf("send IME request: %w", err)
	}
	line, err := r.reader.ReadSlice('\n')
	if err != nil || len(line) > 4096 {
		if err == nil || errors.Is(err, bufio.ErrBufferFull) {
			err = errors.New("IME ACK exceeds 4096 bytes")
		}
		return fmt.Errorf("receive IME ACK: %w", err)
	}
	if err := decodeIMEFrame(line, response); err != nil {
		return fmt.Errorf("invalid IME ACK: %w", err)
	}
	return nil
}

func (r *imeReporter) connect() error {
	if err := validateIMESocket(r.path); err != nil {
		if r.openParams != nil {
			return fmt.Errorf("HERDR_IME_INTENT_INVALID: %w", err)
		}
		return err
	}
	conn, err := net.DialTimeout("unix", r.path, time.Second)
	if err != nil {
		return fmt.Errorf("connect IME transport: %w", err)
	}
	r.conn = conn
	r.reader = bufio.NewReaderSize(conn, 4096)
	if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return err
	}
	if r.openParams == nil {
		return nil
	}
	request := struct {
		ID     string            `json:"id"`
		Method string            `json:"method"`
		Params map[string]string `json:"params"`
	}{"ime:open", "pane.input_intent.stream", r.openParams}
	var response imeOpenResponse
	if err := r.transact(request, &response); err != nil {
		return err
	}
	if response.ID != "ime:open" || response.Error != nil || response.Result == nil {
		return errors.New("Herdr rejected or mismatched input intent stream open")
	}
	result := response.Result
	if result.Type != "pane_input_intent_stream_opened" || result.Session == "" || result.Generation == 0 {
		return errors.New("Herdr returned incomplete input intent stream open")
	}
	r.session, r.generation = result.Session, result.Generation
	return nil
}

// exchange is called only under mu. Inactive is a successful background
// declaration, not authorization; pending is valid only for a direct blur.
func (r *imeReporter) exchange(request imeRequest) (scope string, err error) {
	if r.failure != nil {
		return "", r.failure
	}
	defer func() {
		if err != nil {
			r.failure = err
			r.drop()
		}
	}()
	if r.conn == nil {
		if err = r.connect(); err != nil {
			return "", err
		}
	}
	if err = r.conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return "", err
	}
	var response imeResponse
	if err = r.transact(request, &response); err != nil {
		return "", err
	}
	if !response.OK {
		return "", fmt.Errorf("IME service rejected %s: %s", request.Op, response.Error)
	}
	if response.Error != "" || response.Generation == 0 || response.Session == "" ||
		(r.session != "" && response.Session != r.session) || response.Generation < r.generation {
		return "", errors.New("IME service returned incomplete or stale ACK identity")
	}
	if r.openParams != nil {
		if response.Scope != "recorded" {
			return "", errors.New("Herdr ACK must have recorded scope")
		}
	} else if response.Scope != "applied" && response.Scope != "inactive" &&
		!(response.Scope == "pending" && request.Op == "blur") {
		return "", errors.New("invalid direct IME ACK scope")
	}
	r.session, r.generation = response.Session, response.Generation
	return response.Scope, nil
}

func (r *imeReporter) drop() {
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.conn = nil
	r.reader = nil
	r.active = false
	r.declared = false
	r.state = ""
	r.resumeRequired = false
}

// startEpisode explicitly reasserts the current classifier on startup/focus,
// even if a missing reporter blur left our local active flag stale.
func (r *imeReporter) startEpisode(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startEpisodeLocked(state, false)
}

func (r *imeReporter) key(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startEpisodeLocked(state, true)
}

func (r *imeReporter) startEpisodeLocked(state string, realKey bool) error {
	if r.failure != nil {
		return r.failure
	}
	if r.closed || r.suspended {
		return nil
	}
	if realKey && r.openParams != nil && r.active {
		if r.reader.Buffered() != 0 {
			r.failure = errors.New("unsolicited data on Herdr IME intent stream")
			r.drop()
			return r.failure
		}
		if err := checkIMESocketAlive(r.conn); err != nil {
			r.failure = fmt.Errorf("Herdr IME intent stream disconnected: %w", err)
			r.drop()
			return r.failure
		}
		return r.reportLocked(state)
	}
	request := imeRequest{Op: "activate", State: state, Policy: "mode"}
	if (r.resumeRequired && r.declared) || (realKey && r.openParams == nil && r.active) {
		request = imeRequest{Op: "resume", State: state}
	}
	scope, err := r.exchange(request)
	if err != nil {
		return err
	}
	r.declared = true
	r.state = state
	r.active = scope == "applied" || scope == "recorded"
	r.resumeRequired = false
	return nil
}

func (r *imeReporter) report(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reportLocked(state)
}

func (r *imeReporter) reportLocked(state string) error {
	if r.failure != nil {
		return r.failure
	}
	if r.closed || r.suspended || !r.active || r.state == state {
		return nil
	}
	scope, err := r.exchange(imeRequest{Op: "state", State: state})
	if err != nil {
		return err
	}
	r.state = state
	r.active = scope == "applied" || scope == "recorded"
	return nil
}

func (r *imeReporter) blur() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.suspended || !r.declared {
		return nil
	}
	r.active = false
	_, err := r.exchange(imeRequest{Op: "blur"})
	return err
}

func (r *imeReporter) suspend() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.suspended {
		return nil
	}
	r.active = false
	if r.declared {
		if _, err := r.exchange(imeRequest{Op: "suspend"}); err != nil {
			return err
		}
	}
	r.suspended = true
	r.resumeRequired = true
	return nil
}

// Child completion restores eligibility, not ownership. The next real key or
// focus episode explicitly resumes the parent with its then-current classifier.
func (r *imeReporter) finishSuspension() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspended = false
}

func (r *imeReporter) isActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active && !r.suspended && !r.closed
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
		_, err = r.exchange(imeRequest{Op: "close"})
	}
	r.drop()
	return err
}

// EnableTUIIME is called only by the interactive program path. Herdr transport
// selection precedes ordinary SSH/GUI exclusions in the program entrypoint.
func (m *Model) EnableTUIIME() error {
	if m.imeDisabled || m.imeReporter != nil {
		return nil
	}
	reporter, err := selectIMEReporter()
	if err != nil {
		m.imeDisabled = true
		return nil
	}
	m.imeReporter = reporter
	m.startIMEEpisode()
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
		// Transport validation remains strict; only the optional enhancement
		// stops. Never reconnect or replay this process's previous intent.
		m.imeDisabled = true
		m.imeFocused = false
		if m.imeReporter != nil {
			_ = m.imeReporter.close()
			m.imeReporter = nil
		}
	}
}

func (m *Model) startIMEEpisode() {
	if m.imeReporter != nil {
		err := m.imeReporter.startEpisode(m.imeState())
		m.imeFocused = m.imeReporter.isActive()
		m.noteIMEError(err)
	}
}

func (m *Model) prepareIMEKey() {
	if m.imeReporter != nil {
		// A daemon can pause focus without an app notification. A genuine key
		// rechecks/resumes the same local owner, without activate's snapshot churn.
		err := m.imeReporter.key(m.imeState())
		m.imeFocused = m.imeReporter.isActive()
		m.noteIMEError(err)
	}
}

func (m *Model) reportIME() {
	if m.imeReporter != nil && m.imeFocused {
		err := m.imeReporter.report(m.imeState())
		m.imeFocused = m.imeReporter.isActive()
		m.noteIMEError(err)
	}
}

// CloseIME is called only by the model's lifecycle owner.
func (m *Model) CloseIME() {
	if m.imeReporter != nil {
		m.noteIMEError(m.imeReporter.close())
		m.imeFocused = false
	}
}

// IMEShutdown captures the reporter before the program starts. Signal handlers
// may call the returned function concurrently: it touches only the reporter's
// synchronized lifecycle, never the Model's UI-owned pointer or flags.
func (m *Model) IMEShutdown() func() {
	reporter := m.imeReporter
	return func() {
		if reporter != nil {
			_ = reporter.close()
		}
	}
}
