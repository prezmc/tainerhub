package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Execer is implemented by Docker clients that can open terminals. It's
// separate from Docker so test fakes don't need it.
type Execer interface {
	Exec(ctx context.Context, containerID string, cmd []string, cols, rows int) (*ExecSession, error)
}

// ExecSession is a running TTY exec: read its output, write its input.
type ExecSession struct {
	conn net.Conn
	br   *bufio.Reader
	id   string
	d    *SocketDocker
}

func (e *ExecSession) Read(p []byte) (int, error)  { return e.br.Read(p) }
func (e *ExecSession) Write(p []byte) (int, error) { return e.conn.Write(p) }
func (e *ExecSession) Close() error                { return e.conn.Close() }

func (e *ExecSession) Resize(ctx context.Context, cols, rows int) error {
	resp, err := e.d.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(e.id)+"/resize", url.Values{"h": {strconv.Itoa(rows)}, "w": {strconv.Itoa(cols)}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ExitCode reports how the process ended (after its output has closed).
func (e *ExecSession) ExitCode(ctx context.Context) (int, bool) {
	var r struct {
		Running  bool
		ExitCode int
	}
	if err := e.d.getJSON(ctx, "/exec/"+url.PathEscape(e.id)+"/json", nil, &r); err != nil || r.Running {
		return 0, false
	}
	return r.ExitCode, true
}

// Exec creates a TTY exec in the container and attaches to it. Docker
// "hijacks" the HTTP connection for this: after the 101 response, the same
// socket carries the terminal's raw bytes in both directions.
func (d *SocketDocker) Exec(ctx context.Context, id string, cmd []string, cols, rows int) (*ExecSession, error) {
	if !wire.ValidContainerRef(id) {
		return nil, errors.New("invalid container")
	}
	if len(cmd) == 0 {
		cmd = wire.DefaultShell
	}
	resp, err := d.doBody(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, map[string]any{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": true,
		"Cmd": cmd, "Env": []string{"TERM=xterm-256color"}, "ConsoleSize": []int{rows, cols},
	})
	if err != nil {
		return nil, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	err = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if err != nil || created.ID == "" {
		return nil, errors.New("docker didn't create the exec")
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", d.socket)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"Detach": false, "Tty": true, "ConsoleSize": []int{rows, cols}})
	req := fmt.Sprintf("POST /exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n%s",
		url.PathEscape(created.ID), len(body), body)
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	hr, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if hr.StatusCode != http.StatusSwitchingProtocols && hr.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(hr.Body, 4096))
		conn.Close()
		var e struct{ Message string }
		if json.Unmarshal(msg, &e) == nil && e.Message != "" {
			return nil, fmt.Errorf("docker: %s", e.Message)
		}
		return nil, fmt.Errorf("docker: %s %s", hr.Status, strings.TrimSpace(string(msg)))
	}
	return &ExecSession{conn: conn, br: br, id: created.ID, d: d}, nil
}
