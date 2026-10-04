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
// Replies deliberately model local inactive/application vs Herdr recording.
func imeTestServer(t *testing.T, herdr bool, openReply string, reply func(imeRequest, string, uint64) string) (string, <-chan imeTestEvent) {
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
				if herdr {
					frame, err := reader.ReadBytes('\n')
					if err != nil {
						return
					}
					var open struct {
						ID     string            `json:"id"`
						Method string            `json:"method"`
						Params map[string]string `json:"params"`
					}
					if err := json.Unmarshal(frame, &open); err != nil || open.ID != "ime:open" || open.Method != "pane.input_intent.stream" {
						t.Errorf("invalid stream open: %s (%v)", frame, err)
						return
					}
					events <- imeTestEvent{session: session, params: open.Params}
					response := openReply
					if response == "" {
						response = fmt.Sprintf(`{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":%q,"generation":1}}`, session)
					}
					if _, err := fmt.Fprintln(conn, response); err != nil {
						return
					}
					generation++
				}
				for {
					frame, err := reader.ReadBytes('\n')
					if err != nil {
						return
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
						scope := "applied"
						if herdr {
							scope = "recorded"
						}
						response = fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":%q}`, generation, session, scope)
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

func TestTUIIMEInactiveWaitsForRealKeyAndCurrentClassifier(t *testing.T) {
	path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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
	path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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

func setHerdrIMEEnv(t *testing.T, path string) {
	t.Helper()
	t.Setenv("HERDR_IME_INTENT", "1")
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", path)
	t.Setenv("HERDR_PANE_ID", "pane-fixture")
	t.Setenv("HERDR_IME_POPUP_TERMINAL_ID", "")
	t.Setenv("SSH_CONNECTION", "fixture remote")
	t.Setenv("SSH_TTY", "/dev/pts/fixture")
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
}

func TestHerdrIMEOpensOnlyExplicitPaneOrPopupIntent(t *testing.T) {
	for _, popup := range []bool{false, true} {
		name := "pane"
		if popup {
			name = "popup"
		}
		t.Run(name, func(t *testing.T) {
			path, events := imeTestServer(t, true, "", nil)
			setHerdrIMEEnv(t, path)
			t.Setenv("HOME", filepath.Dir(path)) // no local host daemon here
			field, identity := "pane_id", "pane-fixture"
			if popup {
				t.Setenv("HERDR_PANE_ID", "")
				t.Setenv("HERDR_IME_POPUP_TERMINAL_ID", "popup-fixture")
				field, identity = "popup_terminal_id", "popup-fixture"
			}
			m := NewModel(nil, nil, "")
			defer m.Stop()
			if err := m.EnableTUIIME(); err != nil {
				t.Fatal(err)
			}
			open := expectIMEEvent(t, events, "", "")
			if len(open.params) != 1 || open.params[field] != identity {
				t.Fatalf("stream opened an unintended terminal: %+v", open.params)
			}
			activate := expectIMEEvent(t, events, "activate", "command")
			if activate.session != open.session || !m.imeFocused {
				t.Fatal("recorded intent did not stay on the opened persistent stream")
			}
			m.CloseIME()
			expectIMEEvent(t, events, "close", "")
		})
	}
}

func TestHerdrIMERejectsInvalidLaunchWithoutDirectFallback(t *testing.T) {
	for _, scenario := range []string{"empty-marker", "unsupported", "bad-marker", "missing-env", "no-target", "both-targets", "relative-socket", "socket-symlink", "public-socket", "public-directory"} {
		t.Run(scenario, func(t *testing.T) {
			path, events := imeTestServer(t, true, "", nil)
			setHerdrIMEEnv(t, path)
			switch scenario {
			case "empty-marker":
				t.Setenv("HERDR_IME_INTENT", "")
			case "unsupported":
				t.Setenv("HERDR_IME_INTENT", "unsupported")
			case "bad-marker":
				t.Setenv("HERDR_IME_INTENT", "yes")
			case "missing-env":
				t.Setenv("HERDR_ENV", "")
			case "no-target":
				t.Setenv("HERDR_PANE_ID", "")
			case "both-targets":
				t.Setenv("HERDR_IME_POPUP_TERMINAL_ID", "popup-fixture")
			case "relative-socket":
				t.Setenv("HERDR_SOCKET_PATH", "control.sock")
			case "socket-symlink":
				link := filepath.Join(filepath.Dir(path), "alias.sock")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HERDR_SOCKET_PATH", link)
			case "public-socket":
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "public-directory":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			}
			m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
			defer m.Stop()
			if err := m.EnableTUIIME(); err != nil || !m.imeDisabled || m.imeReporter != nil {
				t.Fatalf("unsafe launch must disable only optional IME: %v", err)
			}
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			if m.list.FilterState() != list.Filtering {
				t.Fatal("invalid marker prevented ordinary search")
			}
			m.Update(tea.FocusMsg{})
			if err := m.EnableTUIIME(); err != nil || m.imeReporter != nil {
				t.Fatal("invalid launch retried its reporter")
			}
			noIMEEvent(t, events)
		})
	}
}

func TestIMEProtocolRejectsBadScopeIdentityAndFramesWithoutReplay(t *testing.T) {
	for _, scenario := range []string{"missing-scope", "recorded-direct", "pending-state", "wrong-session", "backward-generation", "zero-generation", "malformed", "oversized", "unknown-field"} {
		t.Run(scenario, func(t *testing.T) {
			path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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

func TestHerdrIMERejectsIncompleteOpenAndLocalScopes(t *testing.T) {
	for _, scenario := range []string{"wrong-id", "wrong-type", "empty-session", "zero-generation", "error", "oversized-open", "applied", "inactive", "pending", "wrong-session", "stale-generation"} {
		t.Run(scenario, func(t *testing.T) {
			open, scope := "", ""
			switch scenario {
			case "wrong-id":
				open = `{"id":"other","result":{"type":"pane_input_intent_stream_opened","session":"lease-1","generation":1}}`
			case "wrong-type":
				open = `{"id":"ime:open","result":{"type":"ok","session":"lease-1","generation":1}}`
			case "empty-session":
				open = `{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"","generation":1}}`
			case "zero-generation":
				open = `{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"lease-1","generation":0}}`
			case "error":
				open = `{"id":"ime:open","error":{"code":"TARGET_NOT_FOUND","message":"fixture"}}`
			case "oversized-open":
				open = strings.Repeat(" ", 4096) + "{}"
			case "wrong-session":
				scope = "recorded"
			case "stale-generation":
				open = `{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"lease-1","generation":5}}`
				scope = "recorded"
			default:
				scope = scenario
			}
			path, events := imeTestServer(t, true, open, func(request imeRequest, session string, generation uint64) string {
				if scenario == "wrong-session" {
					session = "not-the-opened-stream"
				}
				return fmt.Sprintf(`{"ok":true,"generation":%d,"session":%q,"scope":%q}`, generation, session, scope)
			})
			setHerdrIMEEnv(t, path)
			m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
			defer m.Stop()
			if err := m.EnableTUIIME(); err != nil || !m.imeDisabled || m.imeReporter != nil || m.imeFocused {
				t.Fatal("bad open, scope or identity must disable only optional intent")
			}
			expectIMEEvent(t, events, "", "")
			if scope != "" {
				expectIMEEvent(t, events, "activate", "command")
			}
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			if m.list.FilterState() != list.Filtering {
				t.Fatal("rejected stream prevented ordinary search")
			}
			if err := m.EnableTUIIME(); err != nil || m.imeReporter != nil {
				t.Fatal("bad stream was reopened and replayed")
			}
			noIMEEvent(t, events)
		})
	}
}

func TestHerdrIMEParentChildSuspendKeepsStreamAndWaitsForRealInput(t *testing.T) {
	path, events := imeTestServer(t, true, "", nil)
	setHerdrIMEEnv(t, path)
	m := NewModel([]model.Issue{{ID: "fixture", Title: "fixture", Status: model.StatusOpen}}, nil, "")
	defer m.Stop()
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	parent := expectIMEEvent(t, events, "", "")
	expectIMEEvent(t, events, "activate", "command")
	_, suspend := m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
	if suspend == nil {
		t.Fatal("Ctrl-Z did not hand off the terminal")
	}
	if _, ok := suspend().(tea.SuspendMsg); !ok {
		t.Fatal("Ctrl-Z did not request real process suspension")
	}
	event := expectIMEEvent(t, events, "suspend", "")
	if event.session != parent.session {
		t.Fatal("parent suspension did not retain its original stream")
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
	childOpen := expectIMEEvent(t, events, "", "")
	expectIMEEvent(t, events, "activate", "text")
	if childOpen.session == parent.session {
		t.Fatal("parent and child shared an owner")
	}
	if err := child.close(); err != nil {
		t.Fatal(err)
	}
	expectIMEEvent(t, events, "close", "")
	m.Update(tea.ResumeMsg{})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	noIMEEvent(t, events)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	event = expectIMEEvent(t, events, "resume", "command")
	if event.session != parent.session {
		t.Fatal("parent resume recreated its stream instead of resuming the suspended owner")
	}
	expectIMEEvent(t, events, "state", "text")
	if m.list.FilterState() != list.Filtering {
		t.Fatal("first post-resume key did not dispatch once")
	}
	m.CloseIME()
	expectIMEEvent(t, events, "close", "")
}

func TestTUIIMEEditorCompletionCannotAcquireInBackground(t *testing.T) {
	path, events := imeTestServer(t, false, "", nil)
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
	m.Update(editorExitMsg{tmpFile: file.Name(), err: errors.New("editor cancelled")})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	noIMEEvent(t, events)
	if m.imeFocused || m.imeDisabled {
		t.Fatal("editor completion claimed background command ownership")
	}
	m.Update(tea.FocusMsg{})
	expectIMEEvent(t, events, "resume", "command")
	m.CloseIME()
	expectIMEEvent(t, events, "close", "")
}

func TestTUIIMEPendingBlurDoesNotAuthorizeBackgroundUpdates(t *testing.T) {
	path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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
			path, events := imeTestServer(t, false, "", nil)
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
	path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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
	path, events := imeTestServer(t, false, "", nil)
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
	path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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

func TestHerdrIMECoalescedKeyDisablesEOFAndContinuesUICommand(t *testing.T) {
	path, listener := imeTestListener(t)
	setHerdrIMEEnv(t, path)
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
		if _, err := fmt.Fprintln(conn, `{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"lease-fixture","generation":1}}`); err != nil {
			serverDone <- err
			return
		}
		if _, err := reader.ReadBytes('\n'); err != nil {
			serverDone <- err
			return
		}
		if _, err := fmt.Fprintln(conn, `{"ok":true,"generation":2,"session":"lease-fixture","scope":"recorded"}`); err != nil {
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
		t.Fatal("fixture did not close its intent stream")
	}
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b")})
	if !m.imeDisabled || m.imeReporter != nil || m.imeFocused || m.focused != focusBoard {
		t.Fatal("stream EOF must disable IME and continue ordinary board navigation")
	}
	if command != nil {
		if _, ok := command().(tea.QuitMsg); ok {
			t.Fatal("stream EOF requested Bubble Tea quit")
		}
	}
	m.Update(tea.FocusMsg{})
	m.Update(tea.ResumeMsg{})
	if err := m.EnableTUIIME(); err != nil || m.imeReporter != nil {
		t.Fatal("disabled stream was reopened")
	}
}

func TestTUIIMEStartupMissingContinuesWithoutReplay(t *testing.T) {
	path, events := imeTestServer(t, true, "", nil)
	setHerdrIMEEnv(t, filepath.Join(filepath.Dir(path), "missing.sock"))
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
	t.Setenv("HERDR_SOCKET_PATH", path)
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
			path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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
	path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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
	path, events := imeTestServer(t, false, "", func(request imeRequest, session string, generation uint64) string {
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
