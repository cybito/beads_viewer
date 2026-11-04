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

// imeReporter serializes one interactive app's lifecycle. Under herdr-core the
// hc transport records only intent as a terminal label; ordinary local
// terminals use the daemon that owns the input source. A failed connection is
// never reopened with a cached mode.
type imeReporter struct {
	mu         sync.Mutex
	path       string
	hc         *hcTransport // nil for the ordinary local daemon
	conn       net.Conn
	reader     *bufio.Reader
	session    string
	generation uint64
	state      string
	declared   bool
	active     bool // applied locally, or recorded by herdr-core; never inferred
	suspended  bool // terminal is currently handed to a child
	focusLost  bool // explicit BlurMsg; input and child return cannot override it
	closed     bool
	failure    error
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

// hcTransport writes the imekit.intent label of this process's herdr-core
// terminal (docs/protocol.md in imekit, "reporter：hc transport"). It keeps its
// own held flag and answers the lifecycle with synthetic recorded ACKs.
type hcTransport struct {
	terminal   string
	pgid       int
	held       bool
	nextID     uint64
	generation uint64
}

const hcIntentLabel = "imekit.intent"

type hcLabelResponse struct {
	ID     *uint64         `json:"id"`
	Result *hcLabelResult  `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type hcLabelResult struct {
	Labels map[string]any `json:"labels"`
}

func newIMEReporter(path string) *imeReporter { return &imeReporter{path: path} }

func newHCIMEReporter(path, terminal string, pgid int) *imeReporter {
	return &imeReporter{path: path, hc: &hcTransport{terminal: terminal, pgid: pgid}}
}

func localIMESocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local/state/infra-as-code/ime-control/run/control.sock"), nil
}

func selectIMEReporter() (*imeReporter, error) {
	terminal, socket := os.Getenv("HC_TERMINAL_ID"), os.Getenv("HC_TERMINAL_SOCKET")
	if terminal == "" && socket == "" {
		path, err := localIMESocket()
		if err != nil {
			return nil, err
		}
		return newIMEReporter(path), nil
	}
	if terminal == "" || socket == "" {
		return nil, errors.New("hc transport requires both HC_TERMINAL_ID and HC_TERMINAL_SOCKET")
	}
	pgid, err := imeProcessGroup()
	if err != nil {
		return nil, err
	}
	return newHCIMEReporter(socket, terminal, pgid), nil
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
	line, err := r.roundTrip(request)
	if err != nil {
		return err
	}
	if err := decodeIMEFrame(line, response); err != nil {
		return fmt.Errorf("invalid IME ACK: %w", err)
	}
	return nil
}

func (r *imeReporter) roundTrip(request any) ([]byte, error) {
	frame, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	frame = append(frame, '\n')
	if len(frame) > 4096 {
		return nil, errors.New("IME request exceeds 4096 bytes")
	}
	n, err := r.conn.Write(frame)
	if err != nil || n != len(frame) {
		if err == nil {
			err = errors.New("short IME request write")
		}
		return nil, fmt.Errorf("send IME request: %w", err)
	}
	line, err := r.reader.ReadSlice('\n')
	if err != nil || len(line) > 4096 {
		if err == nil || errors.Is(err, bufio.ErrBufferFull) {
			err = errors.New("IME ACK exceeds 4096 bytes")
		}
		return nil, fmt.Errorf("receive IME ACK: %w", err)
	}
	return line, nil
}

func (r *imeReporter) connect() error {
	// herdr-core leaves its socket 0755; the private parent directory protects it.
	identity, err := validateIMESocket(r.path, r.hc == nil)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", r.path, time.Second)
	if err != nil {
		return fmt.Errorf("connect IME transport: %w", err)
	}
	r.conn = conn
	r.reader = bufio.NewReaderSize(conn, 4096)
	if r.hc != nil {
		if again, err := statIMESocket(r.path); err != nil || again != identity {
			return errors.New("hc socket was replaced while connecting")
		}
	}
	return conn.SetDeadline(time.Now().Add(4 * time.Second))
}

// exchangeHC maps one lifecycle request onto the imekit.intent label. A state
// update while not held is accepted without a write.
func (r *imeReporter) exchangeHC(request imeRequest) (imeResponse, error) {
	h := r.hc
	label := func(state string) (string, error) {
		if state != "command" && state != "text" && state != "idle" {
			return "", fmt.Errorf("invalid IME state %q", state)
		}
		return fmt.Sprintf("1:%d:%s", h.pgid, state), nil
	}
	var value string
	var err error
	write, held := true, h.held
	switch request.Op {
	case "activate", "resume":
		value, err = label(request.State)
		held = true
	case "state":
		write = h.held
		if write {
			value, err = label(request.State)
		}
	case "blur", "suspend":
		value, err = label("idle")
		held = false
	case "close":
		held = false
	default:
		err = fmt.Errorf("unsupported IME lifecycle %q", request.Op)
	}
	if err != nil {
		return imeResponse{}, err
	}
	if write {
		if err := r.writeHCLabel(value); err != nil {
			return imeResponse{}, err
		}
	}
	h.held = held
	h.generation++
	return imeResponse{OK: true, Generation: h.generation, Session: "hc:" + h.terminal, Scope: "recorded"}, nil
}

func (r *imeReporter) writeHCLabel(value string) error {
	h := r.hc
	h.nextID++
	request := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Terminal string            `json:"terminal"`
			Labels   map[string]string `json:"labels"`
		} `json:"params"`
	}{JSONRPC: "2.0", ID: h.nextID, Method: "terminal.label"}
	request.Params.Terminal = h.terminal
	request.Params.Labels = map[string]string{hcIntentLabel: value}
	line, err := r.roundTrip(request)
	if err != nil {
		return err
	}
	var response hcLabelResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("invalid hc reply: %w", err)
	}
	if response.ID == nil || *response.ID != h.nextID || len(response.Error) != 0 ||
		response.Result == nil || response.Result.Labels == nil {
		return errors.New("hc rejected or mismatched terminal.label")
	}
	got, present := response.Result.Labels[hcIntentLabel]
	if value == "" {
		if present {
			return errors.New("hc kept a cleared imekit.intent label")
		}
	} else if got != value {
		return errors.New("hc recorded a different imekit.intent label")
	}
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
	if r.hc != nil {
		response, err = r.exchangeHC(request)
	} else {
		err = r.transact(request, &response)
	}
	if err != nil {
		return "", err
	}
	if !response.OK {
		return "", fmt.Errorf("IME service rejected %s: %s", request.Op, response.Error)
	}
	if response.Error != "" || response.Generation == 0 || response.Session == "" ||
		(r.session != "" && response.Session != r.session) || response.Generation < r.generation {
		return "", errors.New("IME service returned incomplete or stale ACK identity")
	}
	if r.hc != nil {
		if response.Scope != "recorded" {
			return "", errors.New("hc ACK must have recorded scope")
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
	if r.hc != nil {
		r.hc.held = false
	}
	r.active = false
	r.declared = false
	r.state = ""
}

// startEpisode explicitly reasserts the current classifier on startup/focus,
// even if a missing reporter blur left our local active flag stale.
func (r *imeReporter) startEpisode(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Real focus may arrive during a child handoff. Remember it without
	// acquiring ownership until that handoff has finished.
	r.focusLost = false
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
	if r.closed || r.suspended || r.focusLost {
		return nil
	}
	if realKey && r.hc != nil && r.active {
		if r.reader.Buffered() != 0 {
			r.failure = errors.New("unsolicited data on hc connection")
			r.drop()
			return r.failure
		}
		if err := checkIMESocketAlive(r.conn); err != nil {
			r.failure = fmt.Errorf("hc connection lost: %w", err)
			r.drop()
			return r.failure
		}
		return r.reportLocked(state)
	}
	request := imeRequest{Op: "activate", State: state, Policy: "mode"}
	if realKey && r.hc == nil && r.active {
		request = imeRequest{Op: "resume", State: state}
	}
	scope, err := r.exchange(request)
	if err != nil {
		return err
	}
	r.declared = true
	r.state = state
	r.active = scope == "applied" || scope == "recorded"
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
	r.focusLost = true
	r.active = false
	if r.closed || r.suspended || !r.declared {
		return nil
	}
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
	return nil
}

// Terminal return declares this app's current mode, never a child's or parent's.
// A fresh claim handles delayed child cleanup and is subject to focus arbitration.
func (r *imeReporter) finishSuspension(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if r.closed || !r.suspended {
		return nil
	}
	r.suspended = false
	return r.startEpisodeLocked(state, false)
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

// EnableTUIIME is called only by the interactive program path. hc transport
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
