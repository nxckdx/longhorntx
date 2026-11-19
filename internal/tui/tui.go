// Package tui implements the interactive lhtx terminal UI.
package tui

import (
	"context"
	"io"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"k8s.io/klog/v2"

	"github.com/nxckdx/longhorntx/internal/kube"
	"github.com/nxckdx/longhorntx/internal/longhorn"
)

type Options struct {
	Kube      kube.Options
	Namespace string
	Version   string
	RW        bool          // start in write mode
	Refresh   time.Duration // longhorn.RefreshAuto, RefreshWatch or poll interval
}

func Run(ctx context.Context, o Options) error {
	// client-go logs reflector errors to stderr, which would garble the
	// screen. Errors are surfaced in the status bar instead.
	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)

	m := newModel(ctx, o)
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	m.shutdown()
	if err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------- pages

type pageKind int

const (
	pVolumes pageKind = iota
	pNodes
	pActivity
	pDisk
	pDetail
	pSettings
	pHealth
	pContexts
	pHelp
)

// panelKind selects the lower half of the volume detail page.
type panelKind int

const (
	panelNone panelKind = iota
	panelSnapshots
	panelBackups
	panelEvents
	panelJobs
)

type page struct {
	kind   pageKind
	vol    string // pDetail
	node   string // pDisk
	disk   string // pDisk: disk UUID, "" = all disks of node
	cursor int
	offset int

	// pDetail
	release func() // engine watch
	panel   panelKind
	pdata   *panelData
	sel     int // selected replica box
}

type model struct {
	ctx  context.Context
	opts Options

	// connection
	kctx       string
	connecting bool
	connErr    error
	store      *longhorn.Store
	client     *longhorn.Client
	lhVersion  string
	versionErr string

	// data (rebuilt on every change)
	state longhorn.State
	views []longhorn.VolumeView
	nodes []longhorn.NodeView
	acts  []longhorn.Activity

	// navigation
	stack     []*page
	filter    string
	filtering bool

	// context picker
	ctxNames  []string
	ctxCursor int

	// rebuild/backup progress: rate estimation and smooth bars
	rates     *longhorn.RateTracker
	shown     map[string]float64 // Activity.Key() -> displayed percent
	animating bool

	// traffic
	lastRX   int64
	lastTick time.Time
	rxRate   float64

	// write mode, dialogs, status messages
	rw       bool
	modal    *modal
	flash    string
	flashErr bool
	flashAt  time.Time

	// settings page (loaded on demand)
	settings        []*longhorn.Setting
	settingsLoading bool
	settingsErr     error

	// health tab extras (loaded on demand)
	health        *healthExtra
	healthLoading bool
	fcache        []longhorn.Finding

	width, height int
}

func newModel(ctx context.Context, o Options) *model {
	return &model{
		ctx: ctx, opts: o, kctx: o.Kube.Context, connecting: true,
		stack: []*page{{kind: pVolumes}},
		rates: longhorn.NewRateTracker(), shown: map[string]float64{},
		rw: o.RW,
	}
}

func (m *model) top() *page { return m.stack[len(m.stack)-1] }

func (m *model) push(p *page) {
	m.filter, m.filtering = "", false
	m.stack = append(m.stack, p)
}

func (m *model) pop() {
	if len(m.stack) <= 1 {
		return
	}
	p := m.top()
	if p.release != nil {
		p.release()
	}
	m.stack = m.stack[:len(m.stack)-1]
	m.filter, m.filtering = "", false
}

// root replaces the whole stack (tabs 1/2/3).
func (m *model) root(k pageKind) {
	for len(m.stack) > 1 {
		m.pop()
	}
	if m.stack[0].kind != k {
		m.stack[0] = &page{kind: k}
	}
	m.filter, m.filtering = "", false
}

// ---------------------------------------------------------------- messages

type connectedMsg struct {
	kctx   string
	store  *longhorn.Store
	client *longhorn.Client
	err    error
}

type changedMsg struct{ store *longhorn.Store }
type tickMsg time.Time
type animMsg time.Time

// connect builds the client and starts the watches without waiting for any
// data: the UI is usable right away and fills in as lists arrive.
func connect(ctx context.Context, o Options, kctx string) tea.Cmd {
	return func() tea.Msg {
		ko := o.Kube
		ko.Context = kctx
		cl, err := kube.Connect(ko)
		if err != nil {
			return connectedMsg{kctx: kctx, err: err}
		}
		c, err := longhorn.NewClient(cl, o.Namespace)
		if err != nil {
			return connectedMsg{kctx: cl.Context, err: err}
		}
		s := longhorn.NewStore(c)
		s.WatchBackups = true
		s.Refresh = o.Refresh
		s.Start(ctx)
		return connectedMsg{kctx: cl.Context, store: s, client: c}
	}
}

type versionMsg struct {
	client  *longhorn.Client
	version string
	err     error
}

func fetchVersion(ctx context.Context, c *longhorn.Client) tea.Cmd {
	return func() tea.Msg {
		v, err := c.LonghornVersion(ctx)
		return versionMsg{client: c, version: v, err: err}
	}
}

// waitChange blocks until the store reports a change. The short sleep
// afterwards coalesces bursts (the channel holds at most one signal) and
// caps redraws at ~5/s no matter how chatty the cluster is.
func waitChange(s *longhorn.Store) tea.Cmd {
	return func() tea.Msg {
		<-s.Changed()
		time.Sleep(200 * time.Millisecond)
		return changedMsg{store: s}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func anim() tea.Cmd {
	return tea.Tick(50*time.Millisecond, func(t time.Time) tea.Msg { return animMsg(t) })
}

// ---------------------------------------------------------------- lifecycle

func (m *model) Init() tea.Cmd {
	return tea.Batch(connect(m.ctx, m.opts, m.kctx), tick())
}

func (m *model) shutdown() {
	for _, p := range m.stack {
		if p.release != nil {
			p.release()
			p.release = nil
		}
	}
	if m.store != nil {
		m.store.Stop()
	}
}

func (m *model) switchContext(name string) tea.Cmd {
	m.shutdown()
	m.store, m.client = nil, nil
	m.state, m.views, m.nodes, m.acts = longhorn.State{}, nil, nil, nil
	m.kctx = name
	m.connecting, m.connErr = true, nil
	m.lhVersion, m.versionErr = "", ""
	m.stack = []*page{{kind: m.stack[0].kind}}
	m.filter, m.filtering = "", false
	m.lastRX, m.rxRate = 0, 0
	m.shown = map[string]float64{}
	m.rates = longhorn.NewRateTracker()
	m.modal, m.settings, m.settingsErr = nil, nil, nil
	m.health, m.healthLoading, m.fcache = nil, false, nil
	return connect(m.ctx, m.opts, name)
}

// ---------------------------------------------------------------- update

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clamp()
		return m, nil

	case connectedMsg:
		if msg.kctx != m.kctx && m.kctx != "" {
			// result of an abandoned context switch
			if msg.store != nil {
				msg.store.Stop()
			}
			return m, nil
		}
		m.kctx = msg.kctx
		m.connecting = false
		if msg.err != nil {
			m.connErr = msg.err
			return m, nil
		}
		m.store, m.client = msg.store, msg.client
		m.lastRX = 0
		m.refresh()
		return m, tea.Batch(waitChange(m.store), fetchVersion(m.ctx, m.client))

	case versionMsg:
		if msg.client != m.client {
			return m, nil
		}
		m.lhVersion = msg.version
		if msg.err != nil {
			m.versionErr = "version unknown"
		} else if !longhorn.VersionAtLeast(msg.version, longhorn.MinVersion) {
			m.versionErr = "unsupported, need ≥ " + longhorn.MinVersion
		}
		return m, nil

	case changedMsg:
		if msg.store != m.store {
			return m, nil
		}
		m.refresh()
		return m, tea.Batch(waitChange(m.store), m.startAnim())

	case tickMsg:
		if m.client != nil {
			now := time.Time(msg)
			if dt := now.Sub(m.lastTick).Seconds(); !m.lastTick.IsZero() && dt > 0 {
				rx := m.client.Cluster.Traffic.RX()
				m.rxRate = 0.5*m.rxRate + 0.5*float64(rx-m.lastRX)/dt
				m.lastRX = rx
			}
			m.lastTick = now
		}
		// finished backups expire from the activity list
		if m.store != nil && len(m.acts) > 0 {
			m.refresh()
		}
		if m.store != nil && !m.store.AllSynced() {
			return m, tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
		}
		return m, tick()

	case animMsg:
		if m.stepAnim() {
			return m, anim()
		}
		m.animating = false
		return m, nil

	case panelMsg:
		m.applyPanel(msg)
		return m, nil

	case actionDoneMsg:
		return m, m.applyActionDone(msg)

	case healthMsg:
		if msg.client != m.client {
			return m, nil
		}
		m.healthLoading, m.health = false, msg.extra
		m.fcache = m.computeFindings()
		m.clamp()
		return m, nil

	case settingsMsg:
		if msg.client != m.client {
			return m, nil
		}
		m.settingsLoading = false
		m.settings, m.settingsErr = msg.settings, msg.err
		m.clamp()
		return m, nil

	case tea.KeyPressMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *model) refresh() {
	if m.store == nil {
		return
	}
	m.state = m.store.State()
	m.views = longhorn.Build(m.state)
	m.nodes = longhorn.BuildNodes(m.state)
	m.acts = longhorn.BuildActivities(m.views, m.state.Engines, m.store.ActiveBackups())
	m.fcache = m.computeFindings()

	now := time.Now()
	keep := map[string]bool{}
	for _, a := range m.acts {
		if a.Progress < 0 || a.Done {
			continue
		}
		k := a.Key()
		keep[k] = true
		m.rates.Observe(k, a.Progress, a.Total, now)
		if _, ok := m.shown[k]; !ok {
			m.shown[k] = float64(a.Progress)
		}
	}
	m.rates.Forget(keep)
	for k := range m.shown {
		if !keep[k] {
			delete(m.shown, k)
		}
	}
	m.clamp()
}

func (m *model) startAnim() tea.Cmd {
	if m.animating || len(m.shown) == 0 {
		return nil
	}
	m.animating = true
	return anim()
}

// stepAnim eases displayed progress towards the real value. Returns whether
// another frame is needed.
func (m *model) stepAnim() bool {
	more := false
	for _, a := range m.acts {
		k := a.Key()
		cur, ok := m.shown[k]
		if !ok {
			continue
		}
		t := float64(a.Progress)
		if d := t - cur; math.Abs(d) < 0.05 {
			m.shown[k] = t
		} else {
			m.shown[k] = cur + d*0.15
			more = true
		}
	}
	return more
}

// shownPct returns the animated percentage for an activity key.
func (m *model) shownPct(key string, real int) float64 {
	if v, ok := m.shown[key]; ok {
		return v
	}
	return float64(real)
}

// ---------------------------------------------------------------- keys

func (m *model) handleKey(k tea.KeyPressMsg) tea.Cmd {
	key := k.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	if m.modal != nil {
		return m.modalKey(k)
	}
	p := m.top()

	if m.filtering {
		switch key {
		case "enter":
			m.filtering = false
		case "esc":
			m.filtering, m.filter = false, ""
		case "backspace":
			if r := []rune(m.filter); len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
		default:
			if k.Text != "" {
				m.filter += k.Text
			}
		}
		p.cursor, p.offset = 0, 0
		return nil
	}

	switch p.kind {
	case pHelp:
		m.pop()
		return nil
	case pContexts:
		switch key {
		case "up", "k":
			m.ctxCursor = max(m.ctxCursor-1, 0)
		case "down", "j":
			m.ctxCursor = min(m.ctxCursor+1, len(m.ctxNames)-1)
		case "enter":
			if m.ctxCursor < len(m.ctxNames) {
				name := m.ctxNames[m.ctxCursor]
				m.pop()
				if name == m.kctx && m.connErr == nil {
					return nil
				}
				return m.switchContext(name)
			}
		case "esc", "q":
			m.pop()
		}
		return nil
	}

	if cmd, ok := m.actionKey(p, key); ok {
		return cmd
	}

	// global keys
	switch key {
	case "q":
		if len(m.stack) > 1 {
			m.pop()
			return nil
		}
		return tea.Quit
	case "esc", "left", "h", "backspace":
		if m.filter != "" {
			m.filter = ""
			return nil
		}
		m.pop()
		return nil
	case "?":
		m.push(&page{kind: pHelp})
		return nil
	case "1":
		m.root(pVolumes)
		return nil
	case "2":
		m.root(pNodes)
		return nil
	case "3":
		m.root(pActivity)
		return nil
	case "4":
		m.root(pSettings)
		if m.settings == nil && !m.settingsLoading {
			return m.loadSettings()
		}
		return nil
	case "5":
		m.root(pHealth)
		if m.health == nil && !m.healthLoading && m.store != nil && m.store.AllSynced() {
			return m.loadHealth()
		}
		return nil
	case "c":
		names, _, err := m.opts.Kube.Contexts()
		if err != nil {
			m.connErr = err
			return nil
		}
		m.ctxNames, m.ctxCursor = names, 0
		for i, n := range names {
			if n == m.kctx {
				m.ctxCursor = i
			}
		}
		m.push(&page{kind: pContexts})
		return nil
	case "/":
		if p.kind != pDetail || p.panel != panelNone {
			m.filtering = true
		}
		return nil
	}

	if p.kind == pDetail {
		switch key {
		case "[":
			p.sel = max(p.sel-1, 0)
			return nil
		case "]":
			if vv := m.findView(p.vol); vv != nil {
				p.sel = min(p.sel+1, len(vv.Replicas)-1)
			}
			return nil
		}
		if cmd, ok := m.detailKey(p, key); ok {
			return cmd
		}
	}
	if p.kind == pSettings && key == "ctrl+r" {
		return m.loadSettings()
	}
	if p.kind == pHealth && key == "ctrl+r" {
		return m.loadHealth()
	}
	if key == "ctrl+r" && m.store != nil && p.kind != pDetail {
		if m.store.RefreshInterval() > 0 {
			m.store.RefreshNow()
			m.setFlash("reloading volumes …", false)
		} else {
			m.setFlash("volumes are live (watch mode)", false)
		}
		return nil
	}

	// list navigation
	rows := m.rowsFor(p)
	h := m.listHeight()
	switch key {
	case "up", "k":
		p.cursor = m.nextSelectable(rows, p.cursor, -1)
	case "down", "j":
		p.cursor = m.nextSelectable(rows, p.cursor, +1)
	case "pgup", "ctrl+b":
		p.cursor -= h
	case "pgdown", "ctrl+f", "space":
		p.cursor += h
	case "home", "g":
		p.cursor = 0
	case "end", "G":
		p.cursor = len(rows) - 1
	case "enter", "right", "l":
		if p.cursor >= 0 && p.cursor < len(rows) && rows[p.cursor].open != nil {
			return rows[p.cursor].open()
		}
	}
	m.clamp()
	return nil
}

func (m *model) nextSelectable(rows []row, cur, dir int) int {
	for i := cur + dir; i >= 0 && i < len(rows); i += dir {
		if !rows[i].header {
			return i
		}
	}
	return cur
}

func (m *model) openDetail(vol string) tea.Cmd {
	p := &page{kind: pDetail, vol: vol}
	if m.store != nil {
		p.release = m.store.AcquireEngine(vol)
	}
	m.push(p)
	return nil
}

func (m *model) clamp() {
	p := m.top()
	n := len(m.rowsFor(p))
	p.cursor = max(min(p.cursor, n-1), 0)
	h := m.listHeight()
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+h {
		p.offset = p.cursor - h + 1
	}
	p.offset = max(min(p.offset, n-h), 0)
}

// listHeight is the number of list rows that fit on the current page.
func (m *model) listHeight() int {
	p := m.top()
	// header bar + column header + footer
	h := m.height - 3
	if m.filtering || m.filter != "" {
		h--
	}
	switch p.kind {
	case pDisk, pActivity, pSettings, pHealth:
		h -= 2 // title + rule
	case pDetail:
		h -= m.detailTopHeight()
	}
	return max(h, 1)
}

func (m *model) findView(name string) *longhorn.VolumeView {
	for i := range m.views {
		if m.views[i].Name == name {
			return &m.views[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- view

func (m *model) View() tea.View {
	p := m.top()
	var body string
	switch {
	case m.width == 0:
	case p.kind == pHelp:
		body = m.viewHelp()
	case p.kind == pContexts:
		body = m.viewContexts()
	case m.connecting:
		body = sDim.Render("\n  connecting to " + m.ctxLabel() + " …")
	case m.connErr != nil && m.store == nil:
		body = "\n  " + sErr.Render("error: ") + m.connErr.Error() + "\n\n  " + sDim.Render("[c] switch context  [q] quit")
	case !m.pageReady(p):
		body = m.viewLoading()
	case p.kind == pDetail:
		body = m.viewDetail(p)
	default:
		body = m.viewPage(p)
	}
	if m.modal != nil && m.height > 2 {
		body = m.viewModal(m.height - 2)
	}
	content := m.viewHeader() + "\n" + body
	lines := strings.Split(content, "\n")
	if m.height > 0 {
		for len(lines) < m.height-1 {
			lines = append(lines, "")
		}
		if len(lines) > m.height-1 {
			lines = lines[:m.height-1]
		}
	}
	content = strings.Join(lines, "\n") + "\n" + m.viewFooter()

	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = "lhtx · " + m.ctxLabel()
	return v
}

func (m *model) ctxLabel() string {
	if m.kctx == "" {
		return "(current context)"
	}
	return m.kctx
}
