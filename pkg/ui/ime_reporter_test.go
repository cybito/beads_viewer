package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dicklesworthstone/beads_viewer/pkg/model"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

func TestBoardSearchAcceptsChineseTitle(t *testing.T) {
	m := NewModel([]model.Issue{{ID: "zh-1", Title: "中文标题", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	key := func(s string) { m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	key("b")
	key("/")
	if !m.board.IsSearchMode() {
		t.Fatal("board search did not open")
	}
	key("中文")
	if got := m.board.SearchQuery(); got != "中文" {
		t.Fatalf("search query = %q; want 中文", got)
	}
	if got := m.board.SearchMatchCount(); got != 1 {
		t.Fatalf("Chinese title matches = %d; want 1", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if got := m.board.SearchQuery(); got != "中" {
		t.Fatalf("backspace removed bytes rather than a rune: %q", got)
	}
}

// A local socket stub delays each ACK so Update's transition cannot finish
// until the service has confirmed the requested source transition.
func TestTUIIMETransitionsWaitForACK(t *testing.T) {
	path, listener := imeTestListener(t)
	requests := make(chan imeRequest)
	ack := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				serverDone <- err
				return
			}
			var request imeRequest
			if err := json.Unmarshal(line, &request); err != nil {
				serverDone <- err
				return
			}
			requests <- request
			<-ack
			if _, err := fmt.Fprintln(conn, `{"ok":true,"generation":1,"session":"test-lease","scope":"applied"}`); err != nil {
				serverDone <- err
				return
			}
			if request.Op == "close" {
				serverDone <- nil
				return
			}
		}
	}()
	m := NewModel([]model.Issue{{ID: "zh-1", Title: "中文", Status: model.StatusOpen}}, nil, "")
	m.imeReporter = newIMEReporter(path)
	m.imeFocused = true
	defer m.Stop()

	step := func(action func(), transitions ...string) {
		t.Helper()
		done := make(chan struct{})
		go func() { action(); close(done) }()
		for i := 0; i < len(transitions); i += 2 {
			op, state := transitions[i], transitions[i+1]
			select {
			case req := <-requests:
				if req.Op != op || req.State != state {
					t.Fatalf("request = %+v; want op=%s state=%s", req, op, state)
				}
				if op == "activate" && req.Policy != "mode" {
					t.Fatalf("activate policy = %q", req.Policy)
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("missing %s/%s request", op, state)
			}
			select {
			case <-done:
				t.Fatalf("%s returned before ACK", op)
			default:
			}
			ack <- struct{}{}
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("transition did not return after ACK")
		}
	}
	key := func(s string) { m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	step(func() { m.startIMEEpisode(); m.Init() }, "activate", "command")
	step(func() { key("/") }, "resume", "command", "state", "text")
	if m.list.FilterState() != list.Filtering {
		t.Fatal("list filter did not open")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	select {
	case req := <-requests:
		t.Fatalf("duplicate mode request: %+v", req)
	default:
	}
	step(func() { m.Update(tea.BlurMsg{}) }, "blur", "")
	step(func() { m.Update(tea.FocusMsg{}) }, "activate", "text")
	step(func() { m.CloseIME() }, "close", "")
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not receive close")
	}
}

func TestTUIIMEFailureDisablesReporterAndDispatchesCurrentKey(t *testing.T) {
	path, listener := imeTestListener(t)
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadBytes('\n'); err != nil {
			serverDone <- err
			return
		}
		if _, err := fmt.Fprintln(conn, `{"ok":true,"generation":1,"session":"test-lease","scope":"applied"}`); err != nil {
			serverDone <- err
			return
		}
		if _, err := reader.ReadBytes('\n'); err != nil {
			serverDone <- err
			return
		}
		_, err = fmt.Fprintln(conn, `{"ok":false,"generation":2,"error":"BACKEND_UNAVAILABLE"}`)
		serverDone <- err
	}()

	m := NewModel([]model.Issue{{ID: "zh-1", Title: "中文", Status: model.StatusOpen}}, nil, "")
	m.imeReporter = newIMEReporter(path)
	m.imeFocused = true
	defer m.Stop()
	m.startIMEEpisode()
	m.Init()
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !m.imeDisabled || m.imeReporter != nil || m.imeFocused {
		t.Fatal("backend rejection must disable IME without claiming protection")
	}
	if m.list.FilterState() != list.Filtering {
		t.Fatal("backend rejection swallowed the current search key")
	}
	if command != nil {
		if _, ok := command().(tea.QuitMsg); ok {
			t.Fatal("optional IME failure requested quit")
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中")})
	if m.list.FilterValue() != "中" {
		t.Fatal("ordinary text input stopped after IME failure")
	}
	m.Update(tea.FocusMsg{})
	m.Update(tea.ResumeMsg{})
	m.CloseIME()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service did not finish")
	}
}

func TestTUIIMETextReceiversFollowRealKeys(t *testing.T) {
	m := NewModel([]model.Issue{{ID: "zh-1", Title: "中文", Labels: []string{"backend"}, Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	key := func(s string) { m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	escape := func() { m.Update(tea.KeyMsg{Type: tea.KeyEsc}) }
	want := func(state string) {
		t.Helper()
		if got := m.imeState(); got != state {
			t.Fatalf("focus=%v, IME state=%s, want %s", m.focused, got, state)
		}
	}
	want("command")
	key("b")
	want("command")
	key("/")
	want("text")
	escape()
	want("command")
	key("b")
	key("l")
	want("text")
	escape()
	want("command")
	key("t")
	want("text")
	escape()
	want("command")
	makeHistoryReportCurrent(m, createTestHistoryReport())
	key("h")
	want("command")
	key("/")
	want("text")
	escape()
	want("command")
}

func imeTestListener(t *testing.T) (string, net.Listener) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("IME Unix transport requires Darwin or Linux")
	}
	directory, err := os.MkdirTemp("", "bv-ime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path, listener
}

type imeTestEvent struct {
	session string
	request imeRequest
	params  map[string]string
}

// Exercise actual socket framing and persistent per-connection identities.
// Direct replies model local inactive/application. In hc mode the fixture is a
// herdr-core JSON-RPC server: each terminal.label write becomes a "label"
// event whose State is the written imekit.intent value, and reply receives the
// JSON-RPC id as its generation argument.
func imeTestServer(t *testing.T, hc bool, reply func(imeRequest, string, uint64) string) (string, <-chan imeTestEvent) {
	t.Helper()
	path, listener := imeTestListener(t)
	events := make(chan imeTestEvent, 64)
	var mu sync.Mutex
	var connections []net.Conn
	var stopped bool
	var workers sync.WaitGroup
	var sessions atomic.Uint64
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if stopped {
				mu.Unlock()
				conn.Close()
				return
			}
			connections = append(connections, conn)
			mu.Unlock()
			workers.Add(1)
			session := fmt.Sprintf("lease-%d", sessions.Add(1))
			go func() {
				defer workers.Done()
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				reader := bufio.NewReader(conn)
				generation := uint64(1)
				lastID := uint64(0)
				for {
					frame, err := reader.ReadBytes('\n')
					if err != nil {
						return
					}
					if hc {
						var call struct {
							JSONRPC string `json:"jsonrpc"`
							ID      uint64 `json:"id"`
							Method  string `json:"method"`
							Params  struct {
								Terminal string            `json:"terminal"`
								Labels   map[string]string `json:"labels"`
							} `json:"params"`
						}
						value, ok := "", false
						if err := json.Unmarshal(frame, &call); err == nil {
							value, ok = call.Params.Labels["imekit.intent"]
						}
						if err != nil || !ok || len(call.Params.Labels) != 1 || call.JSONRPC != "2.0" ||
							call.Method != "terminal.label" || call.ID <= lastID {
							t.Errorf("invalid terminal.label call: %s (%v)", frame, err)
							return
						}
						lastID = call.ID
						request := imeRequest{Op: "label", State: value}
						events <- imeTestEvent{session: session, request: request, params: map[string]string{"terminal": call.Params.Terminal}}
						response := ""
						if reply != nil {
							response = reply(request, session, call.ID)
						}
						if response == "" {
							labels := "{}"
							if value != "" {
								labels = fmt.Sprintf(`{"imekit.intent":%q}`, value)
							}
							response = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"terminal":%q,"labels":%s}}`, call.ID, call.Params.Terminal, labels)
						}
						if _, err := fmt.Fprintln(conn, response); err != nil {
							return
						}
						continue
					}
					var request imeRequest
					if err := json.Unmarshal(frame, &request); err != nil {
						t.Errorf("invalid lifecycle frame: %s (%v)", frame, err)
						return
					}
					events <- imeTestEvent{session: session, request: request}
					response := ""
					if reply != nil {
						response = reply(request, session, generation)
					}
					if response == "" {
						response = fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":"applied"}`, generation, session)
					}
					if _, err := fmt.Fprintln(conn, response); err != nil {
						return
					}
					generation++
					if request.Op == "close" {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		stopped = true
		for _, conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return path, events
}

func hcTestLabel(t *testing.T, state string) string {
	t.Helper()
	if state == "" {
		return ""
	}
	pgid, err := imeProcessGroup()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("1:%d:%s", pgid, state)
}

func newHCTestReporter(t *testing.T, path string) *imeReporter {
	t.Helper()
	pgid, err := imeProcessGroup()
	if err != nil {
		t.Fatal(err)
	}
	return newHCIMEReporter(path, "t7", pgid)
}

// expectIMELifecycle maps a lifecycle request onto what the hc transport writes.
func expectIMELifecycle(t *testing.T, events <-chan imeTestEvent, hc bool, op, state string) imeTestEvent {
	t.Helper()
	if !hc {
		return expectIMEEvent(t, events, op, state)
	}
	switch op {
	case "blur", "suspend":
		state = "idle"
	case "close":
		state = ""
	}
	return expectIMEEvent(t, events, "label", hcTestLabel(t, state))
}

func expectIMEEvent(t *testing.T, events <-chan imeTestEvent, op, state string) imeTestEvent {
	t.Helper()
	select {
	case event := <-events:
		if event.request.Op != op || event.request.State != state {
			t.Fatalf("IME event = %+v; want %s/%s", event, op, state)
		}
		return event
	case <-time.After(2 * time.Second):
		t.Fatalf("missing IME event %s/%s", op, state)
		return imeTestEvent{}
	}
}

func noIMEEvent(t *testing.T, events <-chan imeTestEvent) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("background update acquired or changed IME ownership: %+v", event)
	default:
	}
}

func TestTUIIMEExplicitBlurBlocksInputUntilFocus(t *testing.T) {
	for _, hc := range []bool{false, true} {
		t.Run(fmt.Sprintf("hc=%v", hc), func(t *testing.T) {
			path, events := imeTestServer(t, hc, nil)
			m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
			defer m.Stop()
			r := newIMEReporter(path)
			if hc {
				r = newHCTestReporter(t, path)
			}
			m.imeReporter = r
			m.startIMEEpisode()
			expectIMELifecycle(t, events, hc, "activate", "command")
			m.Update(tea.BlurMsg{})
			expectIMELifecycle(t, events, hc, "blur", "")
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中"), Paste: true})
			m.Update(tea.MouseMsg{})
			m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
			noIMEEvent(t, events)
			if m.imeFocused || r.isActive() || m.focused == focusBoard {
				t.Fatal("input overrode explicit blur")
			}
			m.Update(tea.FocusMsg{})
			expectIMELifecycle(t, events, hc, "activate", "command")
			if !m.imeFocused {
				t.Fatal("real focus failed to reacquire")
			}
			m.CloseIME()
			expectIMELifecycle(t, events, hc, "close", "")
		})
	}
}

func TestIMEBlurSurvivesSuspensionUntilRealFocus(t *testing.T) {
	for _, hc := range []bool{false, true} {
		for _, focusDuringSuspension := range []bool{false, true} {
			t.Run(fmt.Sprintf("hc=%v/focusDuringSuspension=%v", hc, focusDuringSuspension), func(t *testing.T) {
				path, events := imeTestServer(t, hc, nil)
				r := newIMEReporter(path)
				if hc {
					r = newHCTestReporter(t, path)
				}
				check := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				check(r.startEpisode("command"))
				expectIMELifecycle(t, events, hc, "activate", "command")
				check(r.suspend())
				expectIMELifecycle(t, events, hc, "suspend", "")
				check(r.blur())
				check(r.key("command"))
				if focusDuringSuspension {
					check(r.startEpisode("text"))
				}
				noIMEEvent(t, events)
				check(r.finishSuspension("text"))
				if !focusDuringSuspension {
					check(r.key("command"))
					check(r.report("command"))
					noIMEEvent(t, events)
					if r.isActive() {
						t.Fatal("child return overrode explicit blur")
					}
					check(r.startEpisode("text"))
				}
				expectIMELifecycle(t, events, hc, "activate", "text")
				check(r.close())
				expectIMELifecycle(t, events, hc, "close", "")
			})
		}
	}
}

func TestIMEBlurBeforeDeclarationBlocksInput(t *testing.T) {
	path, events := imeTestServer(t, false, nil)
	r := newIMEReporter(path)
	if err := r.blur(); err != nil {
		t.Fatal(err)
	}
	if err := r.key("command"); err != nil {
		t.Fatal(err)
	}
	noIMEEvent(t, events)
	if r.conn != nil || r.isActive() {
		t.Fatal("key after undeclared blur opened a lease")
	}
	if err := r.startEpisode("command"); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "activate", "command")
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "close", "")
}

func TestIMEExplicitBlurAfterInactiveACKBlocksInput(t *testing.T) {
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		return fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":"inactive"}`, generation, session)
	})
	r := newIMEReporter(path)
	if err := r.startEpisode("command"); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "activate", "command")
	if err := r.blur(); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "blur", "")
	if err := r.key("command"); err != nil {
		t.Fatal(err)
	}
	noIMEEvent(t, events)
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "close", "")
}

func TestTUIIMEInactiveWaitsForRealKeyAndCurrentClassifier(t *testing.T) {
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		if request.Op == "state" {
			return fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":"inactive"}`, generation, session)
		}
		return ""
	})
	m := NewModel([]model.Issue{{ID: "zh-1", Title: "中文", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	m.imeReporter = newIMEReporter(path)
	m.startIMEEpisode()
	expectIMEEvent(t, events, "activate", "command")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	expectIMEEvent(t, events, "resume", "command")
	expectIMEEvent(t, events, "state", "text")
	if m.imeDisabled || m.imeFocused || m.list.FilterState() != list.Filtering {
		t.Fatalf("inactive transition lost UI state or claimed authorization: disabled=%v focus=%v filter=%v", m.imeDisabled, m.imeFocused, m.list.FilterState())
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.Update(embeddedTextInputMsg{})
	noIMEEvent(t, events)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中")})
	expectIMEEvent(t, events, "activate", "text")
	if !m.imeFocused || m.list.FilterValue() != "中" {
		t.Fatalf("real text key did not resume the current classifier: focus=%v text=%q", m.imeFocused, m.list.FilterValue())
	}
	m.CloseIME()
	expectIMEEvent(t, events, "close", "")
}

func TestTUIIMEInactiveCommandDoesNotDispatchOrQuit(t *testing.T) {
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		return fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":"inactive"}`, generation, session)
	})
	m := NewModel([]model.Issue{{ID: "zh-1", Title: "中文", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	m.imeReporter = newIMEReporter(path)
	m.startIMEEpisode()
	expectIMEEvent(t, events, "activate", "command")
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	expectIMEEvent(t, events, "activate", "command")
	if m.imeDisabled || command != nil || m.focused == focusBoard || m.imeFocused {
		t.Fatalf("inactive command was dispatched, authorized or fatal: disabled=%v command=%v focus=%v", m.imeDisabled, command != nil, m.focused)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	noIMEEvent(t, events)
	m.CloseIME()
	expectIMEEvent(t, events, "close", "")
}

// directDaemonCanary points HOME at a valid direct ime-control socket and
// counts connections, proving hc launches never fall back to it.
func directDaemonCanary(t *testing.T) *atomic.Uint64 {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "bvh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if home, err = filepath.EvalSymlinks(home); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	path, err := localIMESocket()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			conn.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return &accepted
}

func setHCIMEEnv(t *testing.T, path string) {
	t.Helper()
	t.Setenv("HC_TERMINAL_ID", "t7")
	t.Setenv("HC_TERMINAL_SOCKET", path)
	t.Setenv("SSH_CONNECTION", "fixture remote")
	t.Setenv("SSH_TTY", "/dev/pts/fixture")
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
}

func TestHCIMESelectionPrecedesDirectDaemon(t *testing.T) {
	path, events := imeTestServer(t, true, nil)
	canary := directDaemonCanary(t)
	t.Setenv("HC_TERMINAL_ID", "")
	t.Setenv("HC_TERMINAL_SOCKET", "")
	direct, err := selectIMEReporter()
	if err != nil || direct.hc != nil {
		t.Fatalf("no hc env must select the direct daemon: %v", err)
	}
	setHCIMEEnv(t, path)
	// herdr-core leaves its socket 0755 inside a private directory.
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	if err := m.EnableTUIIME(); err != nil || m.imeReporter == nil || m.imeReporter.hc == nil || !m.imeFocused {
		t.Fatalf("hc env must select the hc transport before SSH/GUI checks: %v", err)
	}
	first := expectIMEEvent(t, events, "label", hcTestLabel(t, "command"))
	if first.params["terminal"] != "t7" {
		t.Fatalf("label written to the wrong terminal: %+v", first.params)
	}
	if m.imeReporter.session != "hc:t7" || m.imeReporter.generation != 1 {
		t.Fatalf("synthetic ACK identity = %s/%d", m.imeReporter.session, m.imeReporter.generation)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if event := expectIMEEvent(t, events, "label", hcTestLabel(t, "text")); event.session != first.session {
		t.Fatal("hc transport did not keep one persistent connection")
	}
	m.CloseIME()
	expectIMEEvent(t, events, "label", "")
	if canary.Load() != 0 {
		t.Fatal("hc launch contacted the direct daemon")
	}
}

func TestHCIMELifecycleMapping(t *testing.T) {
	path, events := imeTestServer(t, true, nil)
	r := newHCTestReporter(t, path)
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	check(r.startEpisode("command"))
	expectIMEEvent(t, events, "label", hcTestLabel(t, "command"))
	check(r.report("text"))
	expectIMEEvent(t, events, "label", hcTestLabel(t, "text"))
	check(r.suspend())
	expectIMEEvent(t, events, "label", hcTestLabel(t, "idle"))
	check(r.finishSuspension("command"))
	expectIMEEvent(t, events, "label", hcTestLabel(t, "command"))
	check(r.key("command"))
	noIMEEvent(t, events)
	check(r.blur())
	expectIMEEvent(t, events, "label", hcTestLabel(t, "idle"))
	r.mu.Lock()
	scope, err := r.exchange(imeRequest{Op: "state", State: "text"})
	generation := r.generation
	r.mu.Unlock()
	if err != nil || scope != "recorded" {
		t.Fatalf("state while not held must be accepted: %s %v", scope, err)
	}
	noIMEEvent(t, events)
	if generation != 6 {
		t.Fatalf("synthetic generation = %d; want 6", generation)
	}
	check(r.close())
	expectIMEEvent(t, events, "label", "")
	noIMEEvent(t, events)
}

func TestHCIMEPartialOrUnsafeLaunchDisablesWithoutDirectFallback(t *testing.T) {
	for _, scenario := range []string{"only-terminal", "only-socket", "relative-socket", "dot-socket", "socket-symlink", "public-directory", "missing-socket"} {
		t.Run(scenario, func(t *testing.T) {
			path, events := imeTestServer(t, true, nil)
			canary := directDaemonCanary(t)
			setHCIMEEnv(t, path)
			switch scenario {
			case "only-terminal":
				t.Setenv("HC_TERMINAL_SOCKET", "")
			case "only-socket":
				t.Setenv("HC_TERMINAL_ID", "")
			case "relative-socket":
				t.Setenv("HC_TERMINAL_SOCKET", "control.sock")
			case "dot-socket":
				t.Setenv("HC_TERMINAL_SOCKET", filepath.Dir(path)+"/./control.sock")
			case "socket-symlink":
				link := filepath.Join(filepath.Dir(path), "alias.sock")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HC_TERMINAL_SOCKET", link)
			case "public-directory":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			case "missing-socket":
				t.Setenv("HC_TERMINAL_SOCKET", filepath.Join(filepath.Dir(path), "missing.sock"))
			}
			m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
			defer m.Stop()
			if err := m.EnableTUIIME(); err != nil || !m.imeDisabled || m.imeReporter != nil {
				t.Fatalf("unsafe launch must disable only optional IME: %v", err)
			}
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			if m.list.FilterState() != list.Filtering {
				t.Fatal("invalid launch prevented ordinary search")
			}
			m.Update(tea.FocusMsg{})
			if err := m.EnableTUIIME(); err != nil || m.imeReporter != nil {
				t.Fatal("invalid launch retried its reporter")
			}
			noIMEEvent(t, events)
			if canary.Load() != 0 {
				t.Fatal("invalid hc launch fell back to the direct daemon")
			}
		})
	}
}

func TestHCIMERejectsInvalidRepliesWithoutReplay(t *testing.T) {
	for _, scenario := range []string{"error", "wrong-id", "string-id", "no-result", "no-labels", "other-value", "missing-value", "malformed", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			path, events := imeTestServer(t, true, func(request imeRequest, session string, id uint64) string {
				result := fmt.Sprintf(`"result":{"labels":{"imekit.intent":%q}}`, request.State)
				switch scenario {
				case "error":
					return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"fixture"}}`, id)
				case "wrong-id":
					return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,%s}`, id+1, result)
				case "string-id":
					return fmt.Sprintf(`{"jsonrpc":"2.0","id":"%d",%s}`, id, result)
				case "no-result":
					return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d}`, id)
				case "no-labels":
					return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"terminal":"t7"}}`, id)
				case "other-value":
					return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"labels":{"imekit.intent":"1:1:text"}}}`, id)
				case "missing-value":
					return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"labels":{}}}`, id)
				case "malformed":
					return "{"
				case "oversized":
					return strings.Repeat(" ", 4096) + "{}"
				}
				return ""
			})
			setHCIMEEnv(t, path)
			m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
			defer m.Stop()
			if err := m.EnableTUIIME(); err != nil || !m.imeDisabled || m.imeReporter != nil || m.imeFocused {
				t.Fatal("invalid hc reply must disable only optional intent")
			}
			expectIMEEvent(t, events, "label", hcTestLabel(t, "command"))
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			if m.list.FilterState() != list.Filtering {
				t.Fatal("rejected reply prevented ordinary search")
			}
			if err := m.EnableTUIIME(); err != nil || m.imeReporter != nil {
				t.Fatal("failed hc transport was reopened and replayed")
			}
			noIMEEvent(t, events)
		})
	}
}

func TestHCIMECloseRequiresDeletedLabel(t *testing.T) {
	path, events := imeTestServer(t, true, func(request imeRequest, session string, id uint64) string {
		if request.State == "" {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"labels":{"imekit.intent":"stale"}}}`, id)
		}
		return ""
	})
	r := newHCTestReporter(t, path)
	if err := r.startEpisode("command"); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "label", hcTestLabel(t, "command"))
	if err := r.close(); err == nil {
		t.Fatal("close accepted a reply that kept imekit.intent")
	}
	expectIMEEvent(t, events, "label", "")
}

func TestIMEProtocolRejectsBadScopeIdentityAndFramesWithoutReplay(t *testing.T) {
	for _, scenario := range []string{"missing-scope", "recorded-direct", "pending-state", "wrong-session", "backward-generation", "zero-generation", "malformed", "oversized", "unknown-field"} {
		t.Run(scenario, func(t *testing.T) {
			path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
				if generation == 1 {
					return fmt.Sprintf(`{"ok":true,"generation":4,"session":%q,"scope":"applied"}`, session)
				}
				scope, identity, next := "applied", session, uint64(5)
				switch scenario {
				case "missing-scope":
					scope = ""
				case "recorded-direct":
					scope = "recorded"
				case "pending-state":
					scope = "pending"
				case "wrong-session":
					identity = "another-owner"
				case "backward-generation":
					next = 3
				case "zero-generation":
					next = 0
				case "malformed":
					return "{"
				case "oversized":
					return strings.Repeat(" ", 4096) + "{}"
				case "unknown-field":
					return fmt.Sprintf(`{"ok":true,"generation":5,"session":%q,"scope":"applied","source":"forbidden"}`, session)
				}
				return fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":%q}`, next, identity, scope)
			})
			r := newIMEReporter(path)
			if err := r.startEpisode("command"); err != nil {
				t.Fatal(err)
			}
			expectIMEEvent(t, events, "activate", "command")
			if err := r.report("text"); err == nil {
				t.Fatal("bad ACK authorized a mode transition")
			}
			expectIMEEvent(t, events, "state", "text")
			if r.isActive() {
				t.Fatal("bad ACK retained command authorization")
			}
			if err := r.startEpisode("command"); err == nil {
				t.Fatal("failed transport reconnected or replayed a cached mode")
			}
			noIMEEvent(t, events)
			r.close()
		})
	}
}

func TestTUIIMEHandoffReturnDeclaresCurrentModeBeforeInput(t *testing.T) {
	for _, editor := range []bool{false, true} {
		for _, editing := range []bool{false, true} {
			t.Run(fmt.Sprintf("editor=%v/text=%v", editor, editing), func(t *testing.T) {
				path, events := imeTestServer(t, false, nil)
				m := NewModel(nil, nil, "")
				defer m.Stop()
				m.imeReporter = newIMEReporter(path)
				m.startIMEEpisode()
				expectIMEEvent(t, events, "activate", "command")
				if err := m.imeReporter.suspend(); err != nil {
					t.Fatal(err)
				}
				expectIMEEvent(t, events, "suspend", "")
				m.list.SetFilteringEnabled(true)
				if editing {
					m.list.SetFilterState(list.Filtering)
				}
				var completion tea.Msg = tea.ResumeMsg{}
				if editor {
					completion = editorExitMsg{tmpFile: filepath.Join(t.TempDir(), "editor.md"), err: errors.New("editor cancelled")}
				}
				m.Update(completion)
				state := "command"
				if editing {
					state = "text"
				}
				expectIMEEvent(t, events, "activate", state)
				if !m.imeFocused {
					t.Fatal("terminal return did not obtain current acknowledgement before input")
				}
				m.Update(completion)
				m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
				noIMEEvent(t, events)
				m.CloseIME()
				expectIMEEvent(t, events, "close", "")
			})
		}
	}
}

func TestHCIMEParentChildReturnKeepsConnectionAndClaimsBeforeInput(t *testing.T) {
	path, events := imeTestServer(t, true, nil)
	setHCIMEEnv(t, path)
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	parent := expectIMEEvent(t, events, "label", hcTestLabel(t, "command"))
	_, suspend := m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
	if suspend == nil {
		t.Fatal("Ctrl-Z did not hand off the terminal")
	}
	if _, ok := suspend().(tea.SuspendMsg); !ok {
		t.Fatal("Ctrl-Z did not request real process suspension")
	}
	event := expectIMEEvent(t, events, "label", hcTestLabel(t, "idle"))
	if event.session != parent.session {
		t.Fatal("parent suspension did not retain its original connection")
	}
	m.Update(tea.FocusMsg{})
	noIMEEvent(t, events)
	child, err := selectIMEReporter()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.startEpisode("text"); err != nil {
		t.Fatal(err)
	}
	childEvent := expectIMEEvent(t, events, "label", hcTestLabel(t, "text"))
	if childEvent.session == parent.session {
		t.Fatal("parent and child shared a connection")
	}
	if err := child.close(); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "label", "")
	m.Update(tea.ResumeMsg{})
	event = expectIMEEvent(t, events, "label", hcTestLabel(t, "command"))
	if event.session != parent.session {
		t.Fatal("parent resume reconnected instead of reusing its connection")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	noIMEEvent(t, events)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	expectIMEEvent(t, events, "label", hcTestLabel(t, "text"))
	if m.list.FilterState() != list.Filtering {
		t.Fatal("first post-resume key did not dispatch once")
	}
	m.CloseIME()
	expectIMEEvent(t, events, "label", "")
}

func TestTUIIMEEditorCompletionCannotAcquireInBackground(t *testing.T) {
	var foreground atomic.Bool
	foreground.Store(true)
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		if request.Op == "activate" && !foreground.Load() {
			return fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":"inactive"}`, generation, session)
		}
		return ""
	})
	m := NewModel(nil, nil, "")
	defer m.Stop()
	m.imeReporter = newIMEReporter(path)
	m.startIMEEpisode()
	expectIMEEvent(t, events, "activate", "command")
	if err := m.imeReporter.suspend(); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "suspend", "")
	file, err := os.CreateTemp(filepath.Dir(path), "editor-")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	foreground.Store(false)
	m.Update(editorExitMsg{tmpFile: file.Name(), err: errors.New("editor cancelled")})
	expectIMEEvent(t, events, "activate", "command")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	noIMEEvent(t, events)
	if m.imeFocused || m.imeDisabled {
		t.Fatal("editor completion claimed background command ownership")
	}
	foreground.Store(true)
	m.Update(tea.FocusMsg{})
	expectIMEEvent(t, events, "activate", "command")
	m.CloseIME()
	expectIMEEvent(t, events, "close", "")
}

func TestTUIIMEPendingBlurDoesNotAuthorizeBackgroundUpdates(t *testing.T) {
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		if request.Op == "blur" {
			return fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":"pending"}`, generation, session)
		}
		return ""
	})
	m := NewModel(nil, nil, "")
	defer m.Stop()
	m.imeReporter = newIMEReporter(path)
	m.startIMEEpisode()
	expectIMEEvent(t, events, "activate", "command")
	m.Update(tea.BlurMsg{})
	expectIMEEvent(t, events, "blur", "")
	if m.imeDisabled || m.imeFocused || m.imeReporter.isActive() {
		t.Fatal("pending blur was fatal or claimed released/command authorization")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	noIMEEvent(t, events)
	m.Update(tea.FocusMsg{})
	expectIMEEvent(t, events, "activate", "command")
	m.CloseIME()
	expectIMEEvent(t, events, "close", "")
}

func TestIMESocketRejectsAncestorSymlinkAndPublicAncestor(t *testing.T) {
	for _, scenario := range []string{"ancestor-symlink", "public-ancestor"} {
		t.Run(scenario, func(t *testing.T) {
			path, events := imeTestServer(t, false, nil)
			parent := filepath.Dir(path)
			outer := filepath.Join(parent, "outer")
			private := filepath.Join(outer, "private")
			if err := os.MkdirAll(private, 0700); err != nil {
				t.Fatal(err)
			}
			if scenario == "ancestor-symlink" {
				link := filepath.Join(private, "linked")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "control.sock")
			} else {
				nestedPath := filepath.Join(private, "nested.sock")
				listener, err := net.Listen("unix", nestedPath)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				if err := os.Chmod(nestedPath, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(outer, 0777); err != nil {
					t.Fatal(err)
				}
				path = nestedPath
			}
			r := newIMEReporter(path)
			if err := r.startEpisode("command"); err == nil {
				t.Fatal("unsafe socket ancestor granted a command lease")
			}
			if r.conn != nil || r.isActive() {
				t.Fatal("unsafe path was contacted or authorized")
			}
			noIMEEvent(t, events)
		})
	}
}

func TestIMEAcceptsExactly4096ByteACKFrame(t *testing.T) {
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		frame := fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":"applied"}`, generation, session)
		return strings.Repeat(" ", 4095-len(frame)) + frame // final newline is byte 4096
	})
	r := newIMEReporter(path)
	if err := r.startEpisode("command"); err != nil {
		t.Fatalf("maximum valid ACK frame was rejected: %v", err)
	}
	expectIMEEvent(t, events, "activate", "command")
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "close", "")
}

func TestTUIIMERealFocusReassertsCurrentModeWithoutReporterBlur(t *testing.T) {
	path, events := imeTestServer(t, false, nil)
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	m.imeReporter = newIMEReporter(path)
	m.startIMEEpisode()
	expectIMEEvent(t, events, "activate", "command")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	expectIMEEvent(t, events, "resume", "command")
	expectIMEEvent(t, events, "state", "text")
	// The daemon can independently pause its owner after compositor focus loss.
	// A missing app blur must not turn the later real focus into a cached no-op.
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	noIMEEvent(t, events)
	m.Update(tea.FocusMsg{})
	expectIMEEvent(t, events, "activate", "text")
	if !m.imeFocused || m.imeState() != "text" {
		t.Fatal("real focus did not reassert the current text classifier")
	}
	m.CloseIME()
	expectIMEEvent(t, events, "close", "")
}

func TestIMETransportEOFCannotReconnectWithCachedMode(t *testing.T) {
	path, listener := imeTestListener(t)
	var connections atomic.Uint64
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			conn.SetDeadline(time.Now().Add(time.Second))
			bufio.NewReader(conn).ReadBytes('\n')
			conn.Close() // server died before ACK; no cached mode is authorized
		}
	}()
	t.Cleanup(func() { listener.Close(); workers.Wait() })
	r := newIMEReporter(path)
	if err := r.startEpisode("command"); err == nil {
		t.Fatal("EOF before ACK authorized command mode")
	}
	if err := r.startEpisode("text"); err == nil {
		t.Fatal("EOF was silently repaired by replaying a mode")
	}
	if connections.Load() != 1 || r.isActive() {
		t.Fatalf("EOF reporter reconnected or retained authorization: connections=%d active=%v", connections.Load(), r.isActive())
	}
	r.close()
}

func TestTUIIMERealKeyDiscoversObserverOnlyFocusPause(t *testing.T) {
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		if request.Op == "resume" {
			return fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":"inactive"}`, generation, session)
		}
		return ""
	})
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	m.imeReporter = newIMEReporter(path)
	m.startIMEEpisode()
	expectIMEEvent(t, events, "activate", "command")
	// The daemon observer knows focus was lost; the app received no BlurMsg.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	expectIMEEvent(t, events, "resume", "command")
	if m.imeFocused || m.imeDisabled || m.focused == focusBoard {
		t.Fatal("cached authorization survived an inactive real-key focus check")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	noIMEEvent(t, events)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	expectIMEEvent(t, events, "activate", "command")
	if !m.imeFocused || m.focused != focusBoard {
		t.Fatal("new genuine key failed to restart the current command classifier")
	}
	m.CloseIME()
	expectIMEEvent(t, events, "close", "")
}

func TestHCIMECoalescedKeyDisablesEOFAndContinuesUICommand(t *testing.T) {
	path, listener := imeTestListener(t)
	setHCIMEEnv(t, path)
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		if _, err := reader.ReadBytes('\n'); err != nil {
			serverDone <- err
			return
		}
		label, err := imeProcessGroup()
		if err != nil {
			serverDone <- err
			return
		}
		if _, err := fmt.Fprintf(conn, "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"labels\":{\"imekit.intent\":\"1:%d:command\"}}}\n", label); err != nil {
			serverDone <- err
			return
		}
		conn.Close()
		serverDone <- nil
	}()
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fixture did not close its connection")
	}
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if !m.imeDisabled || m.imeReporter != nil || m.imeFocused || m.focused != focusBoard {
		t.Fatal("connection EOF must disable IME and continue ordinary board navigation")
	}
	if command != nil {
		if _, ok := command().(tea.QuitMsg); ok {
			t.Fatal("connection EOF requested Bubble Tea quit")
		}
	}
	m.Update(tea.FocusMsg{})
	m.Update(tea.ResumeMsg{})
	if err := m.EnableTUIIME(); err != nil || m.imeReporter != nil {
		t.Fatal("disabled transport was reopened")
	}
}

func TestTUIIMEStartupMissingContinuesWithoutReplay(t *testing.T) {
	path, events := imeTestServer(t, true, nil)
	setHCIMEEnv(t, filepath.Join(filepath.Dir(path), "missing.sock"))
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	if err := m.EnableTUIIME(); err != nil || !m.imeDisabled || m.imeReporter != nil {
		t.Fatal("missing component must disable optional reporter")
	}
	m.Init()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中")})
	if m.list.FilterState() != list.Filtering || m.list.FilterValue() != "中" {
		t.Fatal("missing component prevented ordinary search")
	}
	// Even a now-valid transport must not revive old process intent.
	t.Setenv("HC_TERMINAL_SOCKET", path)
	m.Update(tea.FocusMsg{})
	m.Update(tea.ResumeMsg{})
	if err := m.EnableTUIIME(); err != nil || m.imeReporter != nil {
		t.Fatal("disabled startup reporter retried")
	}
	m.CloseIME()
	noIMEEvent(t, events)
}

func TestTUIIMERejectedLifecycleRemainsOptional(t *testing.T) {
	for _, op := range []string{"activate", "state", "blur", "suspend", "close"} {
		t.Run(op, func(t *testing.T) {
			path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
				if request.Op == op {
					return fmt.Sprintf(`{"ok":false,"generation":%d,"error":"FOCUS_UNAVAILABLE"}`, generation)
				}
				return ""
			})
			m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
			defer m.Stop()
			r := newIMEReporter(path)
			m.imeReporter = r
			m.startIMEEpisode()
			expectIMEEvent(t, events, "activate", "command")
			switch op {
			case "state":
				m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
				expectIMEEvent(t, events, "resume", "command")
				expectIMEEvent(t, events, "state", "text")
				if m.list.FilterState() != list.Filtering {
					t.Fatal("rejected state lost current search transition")
				}
			case "blur":
				m.Update(tea.BlurMsg{})
				expectIMEEvent(t, events, "blur", "")
			case "suspend":
				_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
				expectIMEEvent(t, events, "suspend", "")
				if cmd == nil {
					t.Fatal("IME rejection prevented terminal suspension")
				}
				if _, ok := cmd().(tea.SuspendMsg); !ok {
					t.Fatal("IME rejection replaced suspension")
				}
			case "close":
				m.CloseIME()
				expectIMEEvent(t, events, "close", "")
			}
			if !m.imeDisabled || m.imeReporter != nil || m.imeFocused || r.isActive() || r.state != "" || r.conn != nil {
				t.Fatal("failure retained live or cached reporter intent")
			}
			m.Update(tea.FocusMsg{})
			m.Update(tea.ResumeMsg{})
			if m.list.FilterState() == list.Filtering {
				m.Update(tea.KeyMsg{Type: tea.KeyEsc})
			}
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
			if m.focused != focusBoard {
				t.Fatal("lifecycle rejection prevented ordinary navigation")
			}
			m.CloseIME()
			noIMEEvent(t, events)
		})
	}
}

type imeEditorProgram struct {
	*Model
	command tea.Cmd
	exited  bool
	err     error
}

func (m *imeEditorProgram) Init() tea.Cmd { return m.command }

func (m *imeEditorProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, command := m.Model.Update(msg)
	if result, ok := msg.(editorExitMsg); ok {
		m.exited, m.err = true, result.err
		return m, tea.Quit
	}
	return m, command
}

func TestTUIIMERejectedEditorHandoffRunsChildAndRecovers(t *testing.T) {
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		if request.Op == "suspend" {
			return `{"ok":false,"generation":2,"error":"BACKEND_UNAVAILABLE"}`
		}
		return ""
	})
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	m.imeReporter = newIMEReporter(path)
	m.startIMEEpisode()
	expectIMEEvent(t, events, "activate", "command")
	marker := filepath.Join(t.TempDir(), "child-ran")
	command := m.launchTerminalEditor([]string{"/bin/sh", "-c", `printf child > "$1"`, "editor-fixture", marker})
	expectIMEEvent(t, events, "suspend", "")
	if command == nil || !m.imeDisabled || m.imeReporter != nil || m.imeFocused {
		t.Fatal("rejected IME handoff prevented child dispatch or retained protection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	programModel := &imeEditorProgram{Model: m, command: command}
	program := tea.NewProgram(programModel, tea.WithContext(ctx), tea.WithInput(strings.NewReader("")), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler())
	if _, err := program.Run(); err != nil {
		t.Fatalf("ordinary child lifecycle failed: %v", err)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "child" || !programModel.exited || programModel.err != nil {
		t.Fatalf("child did not run and return: content=%q read=%v exit=%v child=%v", content, err, programModel.exited, programModel.err)
	}
	m.Update(tea.FocusMsg{})
	m.Update(tea.ResumeMsg{})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if m.focused != focusBoard || m.imeReporter != nil {
		t.Fatal("editor recovery blocked ordinary navigation or replayed IME")
	}
	noIMEEvent(t, events)
}

func TestTUIIMESignalCloseFailureConcurrentWithInputAndFocus(t *testing.T) {
	rejectClose := make(chan struct{})
	path, events := imeTestServer(t, false, func(request imeRequest, session string, generation uint64) string {
		if request.Op == "close" {
			<-rejectClose
			return fmt.Sprintf(`{"ok":false,"generation":%d,"error":"BACKEND_UNAVAILABLE"}`, generation)
		}
		return ""
	})
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	r := newIMEReporter(path)
	m.imeReporter = r
	m.startIMEEpisode()
	expectIMEEvent(t, events, "activate", "command")
	shutdown := m.IMEShutdown()
	signalDone := make(chan struct{})
	go func() {
		shutdown()
		close(signalDone)
	}()
	expectIMEEvent(t, events, "close", "")
	// The signal shutdown holds the reporter mutex while the UI's next
	// genuine key/focus episode attempts to inspect the same reporter.
	uiStarted, uiDone := make(chan struct{}), make(chan struct{})
	go func() {
		close(uiStarted)
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中")})
		m.Update(tea.FocusMsg{})
		m.Update(tea.ResumeMsg{})
		close(uiDone)
	}()
	<-uiStarted
	close(rejectClose)
	for _, done := range []<-chan struct{}{signalDone, uiDone} {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent signal shutdown/UI lifecycle did not finish")
		}
	}
	if !m.imeDisabled || m.imeReporter != nil || m.imeFocused || r.isActive() {
		t.Fatal("signal close failure retained reporter authorization")
	}
	if m.list.FilterState() != list.Filtering || m.list.FilterValue() != "中" {
		t.Fatal("signal close failure swallowed current input")
	}
	// Repeated shutdown still addresses the captured identity after the UI
	// owner has removed its optional reporter; it must not access Model state.
	shutdown()
	noIMEEvent(t, events)
}
