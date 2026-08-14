// Package tui is the Bubble Tea chat client a participant sees after joining a
// room: a scrolling transcript of Claude responses and peer questions, a host
// presence indicator, and an input box. It speaks the relay's participant ws
// protocol; the pure Update logic (event → state) is exercised in model_test.go
// without a socket.
package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// SendFunc writes one raw JSON message to the participant ws. The model calls
// it from a command (never inline) so a slow/failed write can't block Update.
type SendFunc func(payload []byte) error

// speaker kinds drive both grouping and color.
type speaker int

const (
	spkClaude speaker = iota
	spkMe
	spkPeer
	spkSystem
	spkError
)

// chatMsg is one finalized line in the transcript.
type chatMsg struct {
	kind speaker
	name string // display label (peer name / my name); ignored for system/claude
	text string
}

// Model is the participant chat UI.
type Model struct {
	vp    viewport.Model
	input textinput.Model
	send  SendFunc

	myName string

	msgs       []chatMsg
	streaming  string // in-progress Claude response text ("" between turns)
	responding bool   // a turn is live (sent a chat, awaiting done)
	thinking   string // latest thinking summary, shown greyed while responding
	sessionID  string
	hostUp     bool

	width, height int
	ready         bool // received first WindowSizeMsg
	quitting      bool
}

// New builds the initial model. myName is the guest display name; send writes
// to the ws (may be nil in tests that don't exercise input).
func New(myName string, send SendFunc) Model {
	ti := textinput.New()
	ti.Placeholder = "메시지를 입력하고 Enter (Ctrl+C 종료)"
	ti.Focus()
	ti.Prompt = "> "
	ti.CharLimit = 8192
	return Model{
		input:  ti,
		send:   send,
		myName: myName,
	}
}

// Init satisfies tea.Model; the ws pump is started by the caller (run.go), so
// there is no startup command here.
func (m Model) Init() tea.Cmd { return textinput.Blink }

// Update applies one message. Event messages (defined in events.go) mutate
// transcript/streaming/presence state; key messages drive input and sending.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.quitting = true
			return m, tea.Quit
		case tea.KeyEnter:
			text := strings.TrimSpace(m.input.Value())
			if text == "" {
				return m, nil
			}
			m.input.Reset()
			m.msgs = append(m.msgs, chatMsg{kind: spkMe, name: m.myName, text: text})
			m.responding = true
			m.thinking = ""
			m.refresh()
			return m, m.sendChatCmd(text)
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case SessionIDMsg:
		if msg.ID != "" {
			m.sessionID = msg.ID
		}
		return m, nil

	case TextDeltaMsg:
		m.responding = true
		m.streaming += msg.Text
		m.refresh()
		return m, nil

	case ThinkingDeltaMsg:
		m.responding = true
		m.thinking += msg.Text
		m.refresh()
		return m, nil

	case DoneMsg:
		if msg.SessionID != "" {
			m.sessionID = msg.SessionID
		}
		m.finalizeStreaming()
		m.refresh()
		return m, nil

	case ErrorMsg:
		m.finalizeStreaming()
		m.msgs = append(m.msgs, chatMsg{kind: spkError, text: "오류: " + msg.Message})
		m.refresh()
		return m, nil

	case AbortedMsg:
		m.finalizeStreaming()
		m.msgs = append(m.msgs, chatMsg{kind: spkSystem, text: "응답이 중단되었습니다."})
		m.refresh()
		return m, nil

	case PeerChatMsg:
		m.msgs = append(m.msgs, chatMsg{kind: spkPeer, name: speakerName(msg.From), text: msg.Text})
		m.refresh()
		return m, nil

	case HostStatusMsg:
		m.hostUp = msg.Connected
		m.refresh()
		return m, nil

	case UnavailableMsg:
		m.responding = false
		m.msgs = append(m.msgs, chatMsg{kind: spkSystem, text: "호스트가 연결되어 있지 않습니다 — 노트북 데몬이 실행 중인지 확인하세요."})
		m.refresh()
		return m, nil

	case ConnClosedMsg:
		m.hostUp = false
		note := "릴레이 연결이 끊어졌습니다."
		if msg.Err != nil {
			note += " (" + msg.Err.Error() + ")"
		}
		m.msgs = append(m.msgs, chatMsg{kind: spkSystem, text: note})
		m.refresh()
		return m, nil
	}
	return m, nil
}

// finalizeStreaming flushes the in-progress Claude buffer into a finalized
// transcript line and ends the responding state.
func (m *Model) finalizeStreaming() {
	if s := strings.TrimSpace(m.streaming); s != "" {
		m.msgs = append(m.msgs, chatMsg{kind: spkClaude, text: s})
	}
	m.streaming = ""
	m.responding = false
	m.thinking = ""
}

// sendChatCmd builds the outbound chat and writes it via send. from is NOT set
// — the relay injects the verified sub (design policy 4). The field is `prompt`
// because both the executor (req.prompt||req.text) and the relay's peer_chat
// echo (reads top-level "prompt") key off it; sending only "text" would leave
// peers' echoes blank.
func (m Model) sendChatCmd(text string) tea.Cmd {
	payload := buildChat(text, m.sessionID)
	send := m.send
	return func() tea.Msg {
		if send == nil {
			return nil
		}
		if err := send(payload); err != nil {
			return ErrorMsg{Message: "전송 실패: " + err.Error()}
		}
		return nil
	}
}

func buildChat(prompt, sessionID string) []byte {
	obj := map[string]string{"type": "chat", "prompt": prompt}
	if sessionID != "" {
		obj["sessionId"] = sessionID
	}
	b, _ := json.Marshal(obj)
	return b
}

// layout sizes the viewport to fill the space above the header and input rows.
func (m *Model) layout() {
	if m.width <= 0 {
		return
	}
	const headerRows, inputRows = 2, 2 // header+rule, input+hint
	h := m.height - headerRows - inputRows
	if h < 1 {
		h = 1
	}
	m.vp = viewport.New(m.width, h)
	m.input.Width = m.width - 4
}

var (
	styClaude  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // cyan
	styMe      = lipgloss.NewStyle().Foreground(lipgloss.Color("4")) // blue
	styPeer    = lipgloss.NewStyle().Foreground(lipgloss.Color("5")) // magenta
	stySystem  = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // grey
	styError   = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	styThink   = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)
	styHostUp  = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styHostOff = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// refresh re-renders the transcript into the viewport and pins it to the bottom
// so the newest content and live stream stay visible.
func (m *Model) refresh() {
	if !m.ready {
		return
	}
	m.vp.SetContent(m.transcript())
	m.vp.GotoBottom()
}

// transcript renders finalized messages plus any in-progress stream/thinking.
func (m *Model) transcript() string {
	var b strings.Builder
	for _, msg := range m.msgs {
		b.WriteString(renderMsg(msg))
		b.WriteByte('\n')
	}
	if m.thinking != "" {
		b.WriteString(styThink.Render("💭 " + oneLine(m.thinking, 80)))
		b.WriteByte('\n')
	}
	if m.responding {
		body := m.streaming
		if body == "" {
			body = "…"
		}
		b.WriteString(styClaude.Render("Claude") + "  " + body)
		b.WriteByte('\n')
	}
	return b.String()
}

func renderMsg(m chatMsg) string {
	switch m.kind {
	case spkClaude:
		return styClaude.Render("Claude") + "  " + m.text
	case spkMe:
		return styMe.Render(m.name) + "  " + m.text
	case spkPeer:
		return styPeer.Render(m.name) + "  " + m.text
	case spkError:
		return styError.Render(m.text)
	default:
		return stySystem.Render(m.text)
	}
}

// View renders header + transcript + input.
func (m Model) View() string {
	if m.quitting {
		return "안녕히 가세요.\n"
	}
	if !m.ready {
		return "연결 중…\n"
	}
	return strings.Join([]string{m.header(), m.vp.View(), m.footer()}, "\n")
}

func (m Model) header() string {
	dot, label := styHostOff.Render("○"), styHostOff.Render("호스트 끊김")
	if m.hostUp {
		dot, label = styHostUp.Render("●"), styHostUp.Render("호스트 연결")
	}
	return fmt.Sprintf("%s %s", dot, label)
}

func (m Model) footer() string {
	return m.input.View()
}

// oneLine collapses whitespace and truncates to n runes for the thinking line.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
