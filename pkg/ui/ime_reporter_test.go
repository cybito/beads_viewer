package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
	directory, err := os.MkdirTemp("/tmp", "bv-ime-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
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
			if _, err := fmt.Fprintln(conn, `{"ok":true,"generation":1,"session":"test-lease"}`); err != nil {
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

	step := func(action func(), op, state string) {
		t.Helper()
		done := make(chan struct{})
		go func() { action(); close(done) }()
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
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not return after ACK", op)
		}
	}
	key := func(s string) { m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	step(func() { m.Init() }, "activate", "command")
	step(func() { key("/") }, "state", "text")
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

func TestTUIIMEFailureQuitsBeforeAnotherCommandKey(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "bv-ime-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
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
		if _, err := fmt.Fprintln(conn, `{"ok":true,"generation":1,"session":"test-lease"}`); err != nil {
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
	m.Init()
	_, command := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if m.IMEFailure() == nil {
		t.Fatal("backend rejection must not leave the TUI in an unprotected mode")
	}
	if command == nil {
		t.Fatal("backend rejection must terminate the TUI before another key")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("backend rejection did not request Bubble Tea quit")
	}
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
