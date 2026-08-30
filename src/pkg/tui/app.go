// Package tui implements Hive's full-screen terminal dashboard — the
// keyboard-driven operator view over the same dashboard API the web UI and
// hivectl already consume (kubestellar/hive#4907).
//
// This package is the TUI's own root. It deliberately holds no Hive logic: the
// TUI is a second CLIENT of the dashboard API, not a second implementation of
// the fleet model, so everything it displays arrives over the documented HTTP
// contract in dashboard/openapi.json.
//
// T3 (#5004): the frame is a header bar, a 2×2 grid of the four panes from
// pkg/tui/panes, and a footer keybinding strip, per the layout sketch in
// src/docs/design/tui.md §3.
//
// T12 (#5061): the frame is live. A tick every pollInterval issues the client
// fetches that exist and delivers each result to the panes as that pane's own
// message type; see poll.go for the loop, its cadence and its error policy.
// The SSE feed (T13) and the per-pane content (T5/T7/T9/T11) build on it.
//
// T24 (#5138): the frame is size-aware. The grid already re-derived itself from
// the last tea.WindowSizeMsg on every render, so it shares the space at any
// size for free; what T24 adds is the FLOOR. Below minWidth x minHeight the
// grid is not shrunk, it is replaced by a single centred message, per the
// design doc's note on the sketch (src/docs/design/tui.md §3).
package tui

import (
	"fmt"
	"io"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kubestellar/hive/pkg/tui/client"
	"github.com/kubestellar/hive/pkg/tui/panes"
)

// splash is drawn only before the first tea.WindowSizeMsg arrives. It names
// the binding that gets an operator back out, because a full-screen program
// that does not say how to exit is a trap — especially over SSH.
const splash = "Hive TUI (q to quit)"

// paneCount is the grid's four cells. Focus arithmetic uses it so adding a
// pane later cannot silently desynchronize tab cycling from the pane table.
const paneCount = 4

// minWidth and minHeight are the smallest terminal the grid is drawn in.
//
// The numbers come from what the frame needs to be READABLE rather than from
// what it needs to avoid panicking — the cell() clamp already makes any size
// render without crashing. Below this, every cell's interior is a couple of
// columns wide after two borders and a halved terminal, which is a frame that
// draws but says nothing. Showing an operator a stack of empty boxes is worse
// than telling them the window is too small, so this is the floor.
const (
	minWidth  = 60
	minHeight = 20
)

// tooSmallText is the whole below-minimum frame's content. It is derived from
// the constants rather than spelled out, so the numbers an operator is told to
// resize to cannot drift away from the numbers actually enforced.
var tooSmallText = fmt.Sprintf("terminal too small (need at least %dx%d)", minWidth, minHeight)

// Border styles for the grid cells. The focused pane gets a THICK border, not
// only a color change: test and CI environments render through termenv's
// Ascii profile where colors are stripped, so a color-only highlight would be
// invisible exactly where the golden file pins the frame. The thick border
// survives any profile; the color is a refinement on real terminals.
var (
	unfocusedBorder = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240"))
	focusedBorder = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(lipgloss.Color("205"))
	headerStyle = lipgloss.NewStyle().Bold(true)
	footerStyle = lipgloss.NewStyle().Faint(true)
)

// headerText still carries placeholders after T12, and each one is a fact
// about what is not fetched yet rather than an oversight:
//
//   - `hive:` — the hive's name is on GET /api/status (StatusPayload.HiveID),
//     which no merged client method reads. T6's /api/status client decodes the
//     governor slice; until a client call exposes the id, inventing one here
//     would be a guess rendered as a fact.
//   - `governor:` — same endpoint, T6/T7's to fill.
//   - `ws:` — SSE connection state, and the TUI opens no stream yet. T13b owns
//     this field and it stays literally true until then: not connected.
//
// A dash is "not known", which is true; any value polled data does not support
// would be false. T12 populates none of them because nothing it polls carries
// them — /api/agents is the only merged read, and it carries none of the three.
const headerText = "hive: —   governor: —   ws: not connected"

// footerText lists only the bindings that EXIST. The sketch's full strip
// (p pause, m model, K kick, …) documents keys whose tasks have not landed;
// showing them now would advertise actions that silently do nothing. Each
// action task appends its own binding when it wires the key.
const footerText = "tab focus  ? help  q quit"

// model is the root bubbletea model.
//
// It is a VALUE type with value receivers, which is the bubbletea convention
// rather than an accident: Update returns the next model instead of mutating
// the current one, so a message handler can never leave a half-updated frame
// visible if it returns early.
type model struct {
	// width and height track the terminal size reported by tea.WindowSizeMsg.
	// They are zero until the first message arrives — bubbletea sends one at
	// startup, but View can be called before it lands, so View must tolerate
	// zero rather than assume it has been sized.
	width  int
	height int

	// panes holds the grid's cells in reading order: 0 Agents (top-left),
	// 1 Governor (top-right), 2 Tokens (bottom-left), 3 Events
	// (bottom-right) — the numbering the design sketch's [1]..[4] badges use,
	// zero-based.
	panes [paneCount]panes.Pane

	// focus indexes the focused pane. Exactly one pane is always focused;
	// there is no "nothing focused" state to handle everywhere else.
	focus int

	// api is the dashboard client every poll goes through. client.New cannot
	// fail — a bad HIVE_DASHBOARD_URL surfaces as a request error on the first
	// tick rather than as a constructor error the TUI has no frame to render
	// yet — so the model always has one and poll never has to nil-check it.
	api *client.Client

	// helpVisible is whether the help overlay is up. While it is, the overlay
	// swallows EVERY key — including q — so a reader dismissing it cannot
	// accidentally quit the program instead (see Update).
	helpVisible bool

	// interval is this model's poll cadence, defaulting to pollInterval.
	//
	// It is a field rather than a bare constant read for two reasons. Tests
	// drive a whole tick — fetch, delivery, and the re-arm — without waiting
	// five real seconds for it. And T13b needs exactly this knob: once the SSE
	// stream is connected the poll becomes a fallback and should slow down,
	// not keep hammering an endpoint the stream has already superseded.
	interval time.Duration
}

// newModel returns the root model in its initial state. Unexported because the
// program is entered through Run; the tests use it directly to drive the model
// without a terminal.
func newModel() model {
	return model{
		panes: [paneCount]panes.Pane{
			panes.NewAgents(),
			panes.NewGovernor(),
			panes.NewTokens(),
			panes.NewEvents(),
		},
		api:      client.New(),
		interval: pollInterval,
	}
}

// New returns the TUI's root model for embedding in another bubbletea program
// or driving under teatest. The panes' golden test lives next to the panes
// (pkg/tui/panes/testdata, per the design doc's testing convention) and this
// is its entry point; hivectl's own entry stays Run.
func New() tea.Model {
	return newModel()
}

// Init implements tea.Model.
//
// It gathers the panes' initial commands and starts the poll loop: one fetch
// immediately, and the first tick armed for pollInterval later. The immediate
// fetch is what keeps startup honest — without it every pane would show
// "waiting for data" for a full interval while a perfectly reachable dashboard
// sat there answering, and an operator would read that as the TUI being broken.
func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.poll(), m.scheduleTick()}
	for _, p := range m.panes {
		if c := p.Init(); c != nil {
			cmds = append(cmds, c)
		}
	}
	return tea.Batch(cmds...)
}

// Update implements tea.Model.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		// Re-arm BEFORE the fetches are issued, not after they resolve: the
		// loop's cadence must not depend on how long a fetch takes, and a
		// dashboard that never answers must not be able to stop the clock.
		return m, tea.Batch(m.scheduleTick(), m.poll())
	case fetchErrMsg:
		// Swallowed on purpose — see fetchErrMsg's doc comment. Returning here
		// rather than falling through to the broadcast below is the mechanism:
		// the panes never see the error, so they never have to decide whether
		// to clear their data, and the previous frame simply persists.
		return m, nil
	case tea.KeyMsg:
		// The help overlay is modal and dismisses on ANY key, so it is handled
		// before the global bindings rather than as one of them. Order is the
		// whole mechanism: falling through would let "q" quit the program while
		// the reader believed they were closing a dialog, which is the one
		// misfire a help screen must not have. It also makes "?" a toggle for
		// free — the second press lands here.
		if m.helpVisible {
			m.helpVisible = false
			return m, nil
		}
		// KeyMsg.String() normalizes both plain runes ("q") and control
		// combinations ("ctrl+c", "shift+tab") into one comparable form, so
		// the global bindings can be listed together rather than split
		// across a type switch on key type.
		switch msg.String() {
		case "?":
			m.helpVisible = true
			return m, nil
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.focus = (m.focus + 1) % paneCount
			return m, nil
		case "shift+tab":
			// +paneCount-1 rather than -1: keeps the operand positive, so
			// the modulo never sees a negative number to round wrongly.
			m.focus = (m.focus + paneCount - 1) % paneCount
			return m, nil
		}
		// Any other key belongs to the focused pane. The T3 stubs ignore
		// everything, but routing through this seam now is what lets a pane
		// task add j/k selection without touching the app's key handling.
		var cmd tea.Cmd
		m.panes[m.focus], cmd = m.panes[m.focus].Update(msg)
		return m, cmd
	}
	// Non-key messages go to every pane: a poll result or SSE event (T12,
	// T13b) is not addressed to whichever pane happens to be focused.
	var cmds []tea.Cmd
	for i, p := range m.panes {
		next, c := p.Update(msg)
		m.panes[i] = next
		if c != nil {
			cmds = append(cmds, c)
		}
	}
	return m, tea.Batch(cmds...)
}

// View implements tea.Model.
func (m model) View() string {
	if m.width <= 0 || m.height <= 0 {
		// Not sized yet. Return the bare line rather than laying out into a
		// zero-sized box, which would render as an empty frame — a blank
		// screen for however long it takes the first WindowSizeMsg to
		// arrive.
		return splash
	}

	if m.width < minWidth || m.height < minHeight {
		return m.tooSmallView()
	}

	// One line each for header and footer; the grid gets the rest, split
	// into two rows and two columns. The right column and bottom row absorb
	// the odd remainder so the frame always fills the terminal exactly.
	gridH := m.height - 2
	topH := gridH / 2
	botH := gridH - topH
	leftW := m.width / 2
	rightW := m.width - leftW

	cell := func(i, outerW, outerH int) string {
		style := unfocusedBorder
		if i == m.focus {
			style = focusedBorder
		}
		// The border consumes one row/column on every side; the pane
		// renders only the interior. The clamp stays after T24 even though
		// the minimum-size guard now keeps the grid out of the sizes that
		// need it: it is a defence against a later layout change reserving
		// more chrome, not a duplicate of the guard.
		innerW := max(0, outerW-2)
		innerH := max(0, outerH-2)
		return style.Render(m.panes[i].View(innerW, innerH))
	}

	top := lipgloss.JoinHorizontal(lipgloss.Top,
		cell(0, leftW, topH), cell(1, rightW, topH))
	bottom := lipgloss.JoinHorizontal(lipgloss.Top,
		cell(2, leftW, botH), cell(3, rightW, botH))

	header := headerStyle.Width(m.width).Render(headerText)
	footer := footerStyle.Width(m.width).Render(footerText)

	frame := lipgloss.JoinVertical(lipgloss.Left, header, top, bottom, footer)
	if m.helpVisible {
		// Place, not Join: the overlay sits ON the frame rather than taking
		// rows from it, so the grid keeps the exact geometry it had and the
		// frame is still the terminal's size when the overlay is dismissed.
		// panes.Help() sizes itself to its content; centring it is this
		// layer's job, the same split pane.go draws between content and the
		// chrome around it.
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, panes.Help())
	}
	return frame
}

// tooSmallView renders the below-minimum frame: the message alone, centred in
// the terminal, and nothing else.
//
// The message is wrapped to the terminal's width and the result is clipped to
// its exact width and height, so the frame fits ANY size — including one
// narrower than the message itself, which lipgloss.Place alone would happily
// overflow. That matters because this is precisely the path a too-narrow
// terminal takes: a minimum-size message that itself wraps past the right edge
// is the same broken frame it exists to avoid.
func (m model) tooSmallView() string {
	msg := lipgloss.NewStyle().
		Width(m.width).
		Align(lipgloss.Center).
		Render(tooSmallText)
	placed := lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, msg)
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(placed)
}

// Run starts the TUI on this process's own terminal and blocks until the
// operator quits. It returns whatever error bubbletea reports, including the
// failure to open a terminal when the program is run without a TTY.
func Run() error {
	return run(os.Stdin, os.Stdout)
}

// run is Run with its terminal injected.
//
// The split exists so tests can drive the REAL program — the same
// tea.NewProgram call with the same options — over pipes instead of a TTY.
// That matters beyond coverage: teatest builds its own program internally, so a
// teatest-only suite never executes this constructor and would not notice
// WithAltScreen being dropped. Alt-screen is not cosmetic — it is what restores
// the operator's scrollback on exit, so `hivectl tui` leaves the terminal the
// way it found it.
func run(in io.Reader, out io.Writer) error {
	_, err := tea.NewProgram(
		newModel(),
		tea.WithAltScreen(),
		tea.WithInput(in),
		tea.WithOutput(out),
	).Run()
	return err
}
