package tui

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/api"
	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/config"
	"gitlab.futo.org/stonefruit/stonefruit-server/installer/internal/docker"
)

type screen int

const (
	screenWelcome screen = iota
	screenDocker
	screenConfig
	screenPassword
	screenDeploy
	screenSuccess
	screenError
)

type phase struct {
	label  string
	status phaseStatus
}

type phaseStatus int

const (
	phasePending phaseStatus = iota
	phaseRunning
	phaseDone
	phaseFailed
)

// Messages.
type dockerCheckedMsg struct {
	version string
	err     error
}
type phaseDoneMsg struct {
	index int
	err   error
	log   string
	hash  string // filled by the hashing phase so the model can store it
}

// Run launches the TUI. workDir is where the compose file will be
// written/read. Returns an error if the user aborts or something fails;
// nil on successful completion.
func Run(workDir string, defaultPort int, defaultDataPath string) error {
	m := newModel(workDir, defaultPort, defaultDataPath)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return err
	}
	fm := final.(model)
	if fm.fatalErr != nil {
		return fm.fatalErr
	}
	if fm.aborted {
		return fmt.Errorf("aborted")
	}
	return nil
}

type model struct {
	screen   screen
	workDir  string
	aborted  bool
	fatalErr error

	// docker check
	dockerVersion string
	dockerErr     error

	// config form
	portInput     textinput.Model
	dataPathInput textinput.Model
	focused       int
	configErr     string

	// password form
	passwordInput textinput.Model
	confirmInput  textinput.Model
	pwFocused     int
	pwErr         string

	// resolved config (from form submit or parsed existing compose)
	cfg             config.Config
	existingInstall bool
	passwordPlain   string // set from the password screen; hashed in deploy phase
	passwordHash    string // set when we read .env from existing install

	// deploy
	phases       []phase
	currentPhase int
	deployLog    strings.Builder
	spinner      spinner.Model
}

func newModel(workDir string, defaultPort int, defaultDataPath string) model {
	port := textinput.New()
	port.Placeholder = strconv.Itoa(defaultPort)
	port.SetValue(strconv.Itoa(defaultPort))
	port.Prompt = ""
	port.CharLimit = 5
	port.Width = 20
	port.Focus()

	dp := textinput.New()
	dp.Placeholder = defaultDataPath
	dp.SetValue(defaultDataPath)
	dp.Prompt = ""
	dp.CharLimit = 256
	dp.Width = 40

	pw := textinput.New()
	pw.Prompt = ""
	pw.EchoMode = textinput.EchoPassword
	pw.EchoCharacter = '•'
	pw.CharLimit = 128
	pw.Width = 40

	cf := textinput.New()
	cf.Prompt = ""
	cf.EchoMode = textinput.EchoPassword
	cf.EchoCharacter = '•'
	cf.CharLimit = 128
	cf.Width = 40

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(accent)

	return model{
		screen:        screenWelcome,
		workDir:       workDir,
		portInput:     port,
		dataPathInput: dp,
		passwordInput: pw,
		confirmInput:  cf,
		spinner:       sp,
	}
}

// buildPhases constructs the deploy phase list. Skips "Hashing password" when
// we already have a hash from an existing .env.
func buildPhases(needsHash bool) []phase {
	phases := []phase{{label: "Pulling images"}}
	if needsHash {
		phases = append(phases, phase{label: "Hashing password"})
	}
	phases = append(phases,
		phase{label: "Starting containers"},
		phase{label: "Waiting for server"},
	)
	return phases
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Global key handlers.
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "ctrl+c":
			m.aborted = true
			return m, tea.Quit
		case "esc":
			if m.screen == screenError || m.screen == screenSuccess {
				return m, tea.Quit
			}
		}
	}

	switch m.screen {
	case screenWelcome:
		return m.updateWelcome(msg)
	case screenDocker:
		return m.updateDocker(msg)
	case screenConfig:
		return m.updateConfig(msg)
	case screenPassword:
		return m.updatePassword(msg)
	case screenDeploy:
		return m.updateDeploy(msg)
	case screenSuccess, screenError:
		if km, ok := msg.(tea.KeyMsg); ok {
			switch km.String() {
			case "enter", "q":
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m model) View() string {
	var body string
	switch m.screen {
	case screenWelcome:
		body = m.viewWelcome()
	case screenDocker:
		body = m.viewDocker()
	case screenConfig:
		body = m.viewConfig()
	case screenPassword:
		body = m.viewPassword()
	case screenDeploy:
		body = m.viewDeploy()
	case screenSuccess:
		body = m.viewSuccess()
	case screenError:
		body = m.viewError()
	}
	return frameStyle.Render(body) + "\n"
}

// ── Welcome ─────────────────────────────────────────────────────────

func (m model) updateWelcome(msg tea.Msg) (tea.Model, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "enter", " ":
			m.screen = screenDocker
			return m, tea.Batch(m.spinner.Tick, checkDockerCmd())
		}
	}
	return m, nil
}

func (m model) viewWelcome() string {
	lines := []string{
		bannerStyle.Render(banner),
		"",
		titleStyle.Render("Welcome to Stonefruit."),
		"",
		hintStyle.Render("Press Enter to continue · Ctrl-C to quit"),
	}
	return strings.Join(lines, "\n")
}

// ── Docker check ────────────────────────────────────────────────────

func checkDockerCmd() tea.Cmd {
	return func() tea.Msg {
		v, err := docker.CheckDocker()
		return dockerCheckedMsg{version: v, err: err}
	}
}

func (m model) updateDocker(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case dockerCheckedMsg:
		if msg.err != nil {
			m.dockerErr = msg.err
			m.screen = screenError
			return m, nil
		}
		m.dockerVersion = msg.version

		existing, err := docker.ParseExistingCompose(m.workDir)
		if err != nil {
			m.fatalErr = fmt.Errorf("read existing compose: %w", err)
			m.screen = screenError
			return m, nil
		}
		if existing != nil {
			m.cfg = *existing
			m.existingInstall = true
			hash, err := docker.ParseExistingEnv(m.workDir)
			if err != nil {
				m.fatalErr = fmt.Errorf("read existing .env: %w", err)
				m.screen = screenError
				return m, nil
			}
			if hash != "" {
				m.passwordHash = hash
				m.phases = buildPhases(false)
				m.screen = screenDeploy
				return m, m.startDeploy()
			}
			// Compose exists but no password hash — prompt for one.
			m.passwordInput.Focus()
			m.screen = screenPassword
			return m, textinput.Blink
		}
		m.screen = screenConfig
		return m, textinput.Blink
	}
	return m, nil
}

func (m model) viewDocker() string {
	return "Checking Docker... " + m.spinner.View()
}

// ── Config form ─────────────────────────────────────────────────────

func (m model) updateConfig(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "down":
			m.focused = (m.focused + 1) % 2
			m.refocus()
			return m, textinput.Blink
		case "shift+tab", "up":
			m.focused = (m.focused + 1) % 2 // only two fields, toggle
			m.refocus()
			return m, textinput.Blink
		case "enter":
			return m.submitConfig()
		}
	}
	// Forward to focused input.
	if m.focused == 0 {
		m.portInput, cmd = m.portInput.Update(msg)
	} else {
		m.dataPathInput, cmd = m.dataPathInput.Update(msg)
	}
	return m, cmd
}

func (m *model) refocus() {
	if m.focused == 0 {
		m.portInput.Focus()
		m.dataPathInput.Blur()
	} else {
		m.portInput.Blur()
		m.dataPathInput.Focus()
	}
}

func (m model) submitConfig() (tea.Model, tea.Cmd) {
	portStr := strings.TrimSpace(m.portInput.Value())
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		m.configErr = "Port must be a number between 1 and 65535."
		return m, nil
	}
	dataPath := strings.TrimSpace(m.dataPathInput.Value())
	if dataPath == "" {
		m.configErr = "Notes storage directory cannot be empty."
		return m, nil
	}
	pw, err := config.GeneratePassword()
	if err != nil {
		m.fatalErr = fmt.Errorf("generate postgres password: %w", err)
		m.screen = screenError
		return m, nil
	}
	m.cfg = config.Config{Port: port, DataPath: dataPath, PostgresPassword: pw}
	m.configErr = ""

	if err := docker.WriteCompose(m.workDir, m.cfg); err != nil {
		m.fatalErr = fmt.Errorf("write docker-compose.yml: %w", err)
		m.screen = screenError
		return m, nil
	}

	m.passwordInput.Focus()
	m.screen = screenPassword
	return m, textinput.Blink
}

func (m model) viewConfig() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Configure your server") + "\n\n")
	b.WriteString(renderField("Port", m.portInput.View(), m.focused == 0) + "\n")
	b.WriteString(renderField("Notes storage directory", m.dataPathInput.View(), m.focused == 1) + "\n")
	if m.configErr != "" {
		b.WriteString("\n" + dangerStyle.Render(m.configErr) + "\n")
	}
	b.WriteString("\n" + hintStyle.Render("Tab to switch fields · Enter to confirm · Ctrl-C to quit"))
	return b.String()
}

func renderField(label, input string, focused bool) string {
	marker := "  "
	lbl := labelStyle.Render(label)
	if focused {
		marker = titleStyle.Render("› ")
	}
	return marker + lbl + "  " + input
}

// ── Password form ───────────────────────────────────────────────────

func (m model) updatePassword(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "tab", "down":
			m.pwFocused = (m.pwFocused + 1) % 2
			m.refocusPassword()
			return m, textinput.Blink
		case "shift+tab", "up":
			m.pwFocused = (m.pwFocused + 1) % 2
			m.refocusPassword()
			return m, textinput.Blink
		case "enter":
			return m.submitPassword()
		}
	}
	if m.pwFocused == 0 {
		m.passwordInput, cmd = m.passwordInput.Update(msg)
	} else {
		m.confirmInput, cmd = m.confirmInput.Update(msg)
	}
	return m, cmd
}

func (m *model) refocusPassword() {
	if m.pwFocused == 0 {
		m.passwordInput.Focus()
		m.confirmInput.Blur()
	} else {
		m.passwordInput.Blur()
		m.confirmInput.Focus()
	}
}

func (m model) submitPassword() (tea.Model, tea.Cmd) {
	pw := m.passwordInput.Value()
	confirm := m.confirmInput.Value()
	if len(pw) < 8 {
		m.pwErr = "Password must be at least 8 characters."
		return m, nil
	}
	if pw != confirm {
		m.pwErr = "Passwords do not match."
		return m, nil
	}
	m.pwErr = ""
	m.passwordPlain = pw
	m.phases = buildPhases(true)
	m.screen = screenDeploy
	return m, m.startDeploy()
}

func (m model) viewPassword() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Set the admin password") + "\n")
	b.WriteString(hintStyle.Render("Used when connecting a client to this server. At least 8 characters.") + "\n\n")
	b.WriteString(renderField("Password", m.passwordInput.View(), m.pwFocused == 0) + "\n")
	b.WriteString(renderField("Confirm ", m.confirmInput.View(), m.pwFocused == 1) + "\n")
	if m.pwErr != "" {
		b.WriteString("\n" + dangerStyle.Render(m.pwErr) + "\n")
	}
	b.WriteString("\n" + hintStyle.Render("Tab to switch fields · Enter to confirm · Ctrl-C to quit"))
	return b.String()
}

// ── Deploy ──────────────────────────────────────────────────────────

func (m model) startDeploy() tea.Cmd {
	m.phases[0].status = phaseRunning
	return tea.Batch(m.spinner.Tick, m.runPhaseCmd(0))
}

func (m model) runPhaseCmd(index int) tea.Cmd {
	workDir := m.workDir
	cfg := m.cfg
	plain := m.passwordPlain
	label := m.phases[index].label
	return func() tea.Msg {
		var buf bytes.Buffer
		var err error
		var hash string
		switch label {
		case "Pulling images":
			err = docker.ComposePull(workDir, &buf)
		case "Hashing password":
			hash, err = docker.HashPassword(docker.ServerImage, plain)
			if err == nil {
				err = docker.WriteEnvFile(workDir, hash)
			}
		case "Starting containers":
			if cerr := docker.RemoveStaleContainers(workDir, func(s string) { buf.WriteString(s + "\n") }); cerr != nil {
				err = cerr
			} else {
				err = docker.ComposeUp(workDir, &buf)
			}
		case "Waiting for server":
			baseURL := fmt.Sprintf("http://localhost:%d", cfg.Port)
			err = api.WaitForHealthy(baseURL, 90*time.Second)
		}
		return phaseDoneMsg{index: index, err: err, log: buf.String(), hash: hash}
	}
}

func (m model) updateDeploy(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case phaseDoneMsg:
		if msg.log != "" {
			m.deployLog.WriteString(msg.log)
		}
		if msg.hash != "" {
			m.passwordHash = msg.hash
		}
		if msg.err != nil {
			m.phases[msg.index].status = phaseFailed
			m.fatalErr = fmt.Errorf("%s: %w", m.phases[msg.index].label, msg.err)
			m.screen = screenError
			return m, nil
		}
		m.phases[msg.index].status = phaseDone
		next := msg.index + 1
		if next >= len(m.phases) {
			m.screen = screenSuccess
			return m, nil
		}
		m.currentPhase = next
		m.phases[next].status = phaseRunning
		return m, m.runPhaseCmd(next)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) viewDeploy() string {
	var b strings.Builder
	title := "Installing Stonefruit"
	if m.existingInstall {
		title = "Updating Stonefruit"
	}
	b.WriteString(titleStyle.Render(title) + "\n\n")
	for _, p := range m.phases {
		var icon string
		switch p.status {
		case phasePending:
			icon = mutedStyle.Render(" ·")
		case phaseRunning:
			icon = m.spinner.View()
		case phaseDone:
			icon = " " + checkMark
		case phaseFailed:
			icon = " " + crossMark
		}
		label := p.label
		if p.status == phasePending {
			label = mutedStyle.Render(label)
		}
		fmt.Fprintf(&b, "  %s  %s\n", icon, label)
	}
	return b.String()
}

// ── Success ─────────────────────────────────────────────────────────

func (m model) viewSuccess() string {
	url := fmt.Sprintf("http://localhost:%d", m.cfg.Port)
	var b strings.Builder
	b.WriteString(successStyle.Render("✓ Stonefruit is running.") + "\n\n")
	if m.existingInstall {
		b.WriteString("Updated and restarted.\n\n")
	}
	b.WriteString(labelStyle.Render("Connect the app") + "\n")
	b.WriteString("  Settings → Sync in Stonefruit:\n")
	b.WriteString(fmt.Sprintf("    Server URL:  %s\n", url))
	b.WriteString("    Password:    (the password you just set)\n\n")
	b.WriteString(hintStyle.Render("Press Enter to exit"))
	return b.String()
}

// ── Error ───────────────────────────────────────────────────────────

func (m model) viewError() string {
	var b strings.Builder
	b.WriteString(dangerStyle.Render("✗ Setup failed.") + "\n\n")
	if m.dockerErr != nil {
		b.WriteString(m.dockerErr.Error() + "\n\n")
		b.WriteString("Install Docker from https://docs.docker.com/get-docker/ and try again.\n")
	} else if m.fatalErr != nil {
		b.WriteString(m.fatalErr.Error() + "\n")
		if log := strings.TrimSpace(m.deployLog.String()); log != "" {
			b.WriteString("\n" + hintStyle.Render("─── docker output ───") + "\n" + log + "\n")
		}
	}
	b.WriteString("\n" + hintStyle.Render("Press Enter to exit"))
	return b.String()
}
