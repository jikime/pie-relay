package tui

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/coder/websocket"
)

// maxMessageBytes mirrors the relay's 16 MiB read limit — a large tool_result
// or catalog broadcast must not exceed the client read limit and kill the ws.
const maxMessageBytes = 16 << 20

// Run dials the relay's /ws/participant leg with the participant token and runs
// the chat TUI until the user quits (Ctrl+C) or the socket closes. wsURL is the
// full ws(s)://host/ws/participant endpoint; token is the participant JWT from
// /rooms/join. myName is the display label for the local user's own messages.
func Run(ctx context.Context, wsURL, token, myName string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		if resp != nil {
			return fmt.Errorf("참가 실패 (HTTP %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("릴레이 연결 실패: %w", err)
	}
	c.SetReadLimit(maxMessageBytes)
	defer c.Close(websocket.StatusNormalClosure, "")

	// SendFunc: serialize each outbound chat as a text frame. Called from a
	// tea.Cmd goroutine; coder/websocket.Write is safe for one writer at a time,
	// and the model issues writes one Enter at a time.
	send := func(payload []byte) error {
		return c.Write(ctx, websocket.MessageText, payload)
	}

	p := tea.NewProgram(New(myName, send), tea.WithContext(ctx), tea.WithAltScreen())

	// ws → program: read frames, parse to tea.Msg, deliver via p.Send. On read
	// error (socket closed / ctx done) push one ConnClosedMsg and stop the UI.
	go func() {
		for {
			_, data, rerr := c.Read(ctx)
			if rerr != nil {
				p.Send(ConnClosedMsg{Err: rerr})
				return
			}
			if msg := parseServerEvent(data); msg != nil {
				p.Send(msg)
			}
		}
	}()

	_, err = p.Run()
	return err
}
