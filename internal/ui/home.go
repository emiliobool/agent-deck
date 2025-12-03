package ui

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// ansiRegex matches ANSI escape codes for stripping
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Home is the main application model
type Home struct {
	// Dimensions
	width  int
	height int

	// Data
	instances []*session.Instance
	storage   *session.Storage
	groupTree *session.GroupTree
	flatItems []session.Item // Flattened view for cursor navigation

	// Components
	search      *Search
	newDialog   *NewDialog
	groupDialog *GroupDialog // For creating/renaming groups

	// State
	cursor     int // Selected item index in flatItems
	viewOffset int // First visible item index (for scrolling)
	err        error

	// Context for cleanup
	ctx    context.Context
	cancel context.CancelFunc
}

// Messages
type loadSessionsMsg struct {
	instances []*session.Instance
	groups    []*session.GroupData
	err       error
}

type sessionCreatedMsg struct {
	instance *session.Instance
	err      error
}

type refreshMsg struct{}

type statusUpdateMsg struct{} // Triggers immediate status update without reloading

type tickMsg time.Time

// NewHome creates a new home model
func NewHome() *Home {
	ctx, cancel := context.WithCancel(context.Background())

	storage, err := session.NewStorage()
	if err != nil {
		// Fallback to nil storage - will be handled gracefully
		storage = nil
	}

	return &Home{
		storage:     storage,
		search:      NewSearch(),
		newDialog:   NewNewDialog(),
		groupDialog: NewGroupDialog(),
		cursor:      0,
		ctx:         ctx,
		cancel:      cancel,
		instances:   []*session.Instance{},
		groupTree:   session.NewGroupTree([]*session.Instance{}),
		flatItems:   []session.Item{},
	}
}

// rebuildFlatItems rebuilds the flattened view from group tree
func (h *Home) rebuildFlatItems() {
	h.flatItems = h.groupTree.Flatten()
	// Ensure cursor is valid
	if h.cursor >= len(h.flatItems) {
		h.cursor = len(h.flatItems) - 1
	}
	if h.cursor < 0 {
		h.cursor = 0
	}
	// Adjust viewport if cursor is out of view
	h.syncViewport()
}

// syncViewport ensures the cursor is visible within the viewport
// Call this after any cursor movement
func (h *Home) syncViewport() {
	if len(h.flatItems) == 0 {
		h.viewOffset = 0
		return
	}

	// Calculate visible height for session list
	// Header takes 2 lines, help bar takes 3 lines, content area needs -2 for title
	helpBarHeight := 3
	contentHeight := h.height - 2 - helpBarHeight
	visibleHeight := contentHeight - 2 // -2 for SESSIONS title
	if visibleHeight < 1 {
		visibleHeight = 1
	}

	// If cursor is above viewport, scroll up
	if h.cursor < h.viewOffset {
		h.viewOffset = h.cursor
	}

	// If cursor is below viewport, scroll down
	// Leave room for "⋮ +N more" indicator
	maxVisible := visibleHeight - 1
	if maxVisible < 1 {
		maxVisible = 1
	}
	if h.cursor >= h.viewOffset+maxVisible {
		h.viewOffset = h.cursor - maxVisible + 1
	}

	// Clamp viewOffset to valid range
	maxOffset := len(h.flatItems) - maxVisible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if h.viewOffset > maxOffset {
		h.viewOffset = maxOffset
	}
	if h.viewOffset < 0 {
		h.viewOffset = 0
	}
}

// Init initializes the model
func (h *Home) Init() tea.Cmd {
	return tea.Batch(
		h.loadSessions,
		h.tick(),
	)
}

// loadSessions loads sessions from storage
func (h *Home) loadSessions() tea.Msg {
	if h.storage == nil {
		return loadSessionsMsg{instances: []*session.Instance{}, err: fmt.Errorf("storage not initialized")}
	}

	instances, groups, err := h.storage.LoadWithGroups()
	return loadSessionsMsg{instances: instances, groups: groups, err: err}
}

// tick returns a command that sends a tick message every 500ms
// Status updates use time-based cooldown to prevent flickering
func (h *Home) tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Update handles messages
func (h *Home) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		h.width = msg.Width
		h.height = msg.Height
		h.updateSizes()
		h.syncViewport() // Recalculate viewport when window size changes
		return h, nil

	case loadSessionsMsg:
		if msg.err != nil {
			h.err = msg.err
		} else {
			h.instances = msg.instances
			// Preserve existing group tree structure if it exists
			// Only create new tree on initial load (when groupTree has no groups)
			if h.groupTree.GroupCount() == 0 {
				// Initial load - use stored groups if available
				if len(msg.groups) > 0 {
					h.groupTree = session.NewGroupTreeWithGroups(h.instances, msg.groups)
				} else {
					h.groupTree = session.NewGroupTree(h.instances)
				}
			} else {
				// Refresh - update existing tree with loaded sessions
				h.groupTree.SyncWithInstances(h.instances)
			}
			h.rebuildFlatItems()
			h.search.SetItems(h.instances)
		}
		return h, nil

	case sessionCreatedMsg:
		if msg.err != nil {
			h.err = msg.err
		} else {
			h.instances = append(h.instances, msg.instance)
			// Add to existing group tree instead of rebuilding
			h.groupTree.AddSession(msg.instance)
			h.rebuildFlatItems()
			h.search.SetItems(h.instances)
			if h.storage != nil {
				h.storage.Save(h.instances)
			}
		}
		return h, nil

	case sessionDeletedMsg:
		// Find and remove from list
		var deletedInstance *session.Instance
		for i, s := range h.instances {
			if s.ID == msg.deletedID {
				deletedInstance = s
				h.instances = append(h.instances[:i], h.instances[i+1:]...)
				break
			}
		}
		// Remove from group tree (preserves empty groups)
		if deletedInstance != nil {
			h.groupTree.RemoveSession(deletedInstance)
		}
		h.rebuildFlatItems()
		// Update search items
		h.search.SetItems(h.instances)
		// Save to storage
		if h.storage != nil {
			h.storage.Save(h.instances)
		}
		return h, nil

	case refreshMsg:
		return h, h.loadSessions

	case statusUpdateMsg:
		// Immediate status update without reloading from storage
		// Used when returning from attached session
		for _, inst := range h.instances {
			if err := inst.UpdateStatus(); err != nil {
				// Log error but don't fail - other sessions still need updating
				h.err = fmt.Errorf("status update failed for %s: %w", inst.Title, err)
			}
		}
		// Save state after returning from attached session to persist acknowledged state
		h.saveInstances()
		return h, nil

	case tickMsg:
		// Update status of all sessions every 500ms
		for _, inst := range h.instances {
			if err := inst.UpdateStatus(); err != nil {
				// Log error but don't fail the tick loop
				// Clear previous errors to avoid stale error messages
				if h.err == nil {
					h.err = fmt.Errorf("status update failed for %s: %w", inst.Title, err)
				}
			}
		}
		return h, h.tick()

	case tea.KeyMsg:
		// Handle overlays first
		if h.search.IsVisible() {
			return h.handleSearchKey(msg)
		}
		if h.newDialog.IsVisible() {
			return h.handleNewDialogKey(msg)
		}
		if h.groupDialog.IsVisible() {
			return h.handleGroupDialogKey(msg)
		}

		// Main view keys
		return h.handleMainKey(msg)
	}

	return h, tea.Batch(cmds...)
}

// handleSearchKey handles keys when search is visible
func (h *Home) handleSearchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		selected := h.search.Selected()
		if selected != nil {
			// Find index and select
			for i, inst := range h.instances {
				if inst.ID == selected.ID {
					h.cursor = i
					break
				}
			}
		}
		h.search.Hide()
		return h, nil
	case "esc":
		h.search.Hide()
		return h, nil
	}

	cmd := h.search.Update(msg)
	return h, cmd
}

// handleNewDialogKey handles keys when new dialog is visible
func (h *Home) handleNewDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		// Create session (enter works from any field)
		name, path, command := h.newDialog.GetValues()
		if name != "" && path != "" {
			groupPath := h.newDialog.GetSelectedGroup()
			h.newDialog.Hide()
			return h, h.createSessionInGroup(name, path, command, groupPath)
		}
	case "esc":
		h.newDialog.Hide()
		return h, nil
	}

	var cmd tea.Cmd
	h.newDialog, cmd = h.newDialog.Update(msg)
	return h, cmd
}

// handleMainKey handles keys in main view
func (h *Home) handleMainKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		h.cancel()
		if h.storage != nil {
			h.storage.Save(h.instances)
		}
		return h, tea.Quit

	case "up", "k":
		if h.cursor > 0 {
			h.cursor--
			h.syncViewport()
		}
		return h, nil

	case "down", "j":
		if h.cursor < len(h.flatItems)-1 {
			h.cursor++
			h.syncViewport()
		}
		return h, nil

	case "enter":
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeSession && item.Session != nil {
				if item.Session.Exists() {
					return h, h.attachSession(item.Session)
				}
			} else if item.Type == session.ItemTypeGroup {
				// Toggle group on enter
				h.groupTree.ToggleGroup(item.Path)
				h.rebuildFlatItems()
			}
		}
		return h, nil

	case "tab", "l", "right":
		// Expand/collapse group or expand if on session
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeGroup {
				h.groupTree.ToggleGroup(item.Path)
				h.rebuildFlatItems()
			}
		}
		return h, nil

	case "h", "left":
		// Collapse group
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeGroup {
				h.groupTree.CollapseGroup(item.Path)
				h.rebuildFlatItems()
			} else if item.Type == session.ItemTypeSession {
				// Move cursor to parent group
				h.groupTree.CollapseGroup(item.Path)
				h.rebuildFlatItems()
				// Find the group in flatItems
				for i, fi := range h.flatItems {
					if fi.Type == session.ItemTypeGroup && fi.Path == item.Path {
						h.cursor = i
						break
					}
				}
			}
		}
		return h, nil

	case "shift+up", "K":
		// Move item up
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeGroup {
				h.groupTree.MoveGroupUp(item.Path)
			} else if item.Type == session.ItemTypeSession {
				h.groupTree.MoveSessionUp(item.Session)
			}
			h.rebuildFlatItems()
			if h.cursor > 0 {
				h.cursor--
			}
			h.saveInstances()
		}
		return h, nil

	case "shift+down", "J":
		// Move item down
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeGroup {
				h.groupTree.MoveGroupDown(item.Path)
			} else if item.Type == session.ItemTypeSession {
				h.groupTree.MoveSessionDown(item.Session)
			}
			h.rebuildFlatItems()
			if h.cursor < len(h.flatItems)-1 {
				h.cursor++
			}
			h.saveInstances()
		}
		return h, nil

	case "m":
		// Move session to different group
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeSession {
				h.groupDialog.ShowMove(h.groupTree.GetGroupNames())
			}
		}
		return h, nil

	case "g":
		// Create new group (or subgroup if a group is selected)
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeGroup {
				// Create subgroup under selected group
				h.groupDialog.ShowCreateSubgroup(item.Group.Path, item.Group.Name)
				return h, nil
			}
		}
		// Create root-level group
		h.groupDialog.Show()
		return h, nil

	case "R", "shift+r":
		// Rename group or session
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeGroup {
				h.groupDialog.ShowRename(item.Path, item.Group.Name)
			} else if item.Type == session.ItemTypeSession && item.Session != nil {
				h.groupDialog.ShowRenameSession(item.Session.ID, item.Session.Title)
			}
		}
		return h, nil

	case "/":
		h.search.Show()
		return h, nil

	case "n":
		// Auto-select parent group from current cursor position
		groupPath := "default"
		groupName := "default"
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeGroup {
				groupPath = item.Group.Path
				groupName = item.Group.Name
			} else if item.Type == session.ItemTypeSession {
				// Use the session's group
				groupPath = item.Path
				if group, exists := h.groupTree.Groups[groupPath]; exists {
					groupName = group.Name
				}
			}
		}
		h.newDialog.ShowInGroup(groupPath, groupName)
		return h, nil

	case "d":
		if h.cursor < len(h.flatItems) {
			item := h.flatItems[h.cursor]
			if item.Type == session.ItemTypeSession && item.Session != nil {
				return h, h.deleteSession(item.Session)
			} else if item.Type == session.ItemTypeGroup {
				// Delete group (moves sessions to default)
				h.groupTree.DeleteGroup(item.Path)
				h.instances = h.groupTree.GetAllInstances()
				h.rebuildFlatItems()
				h.saveInstances()
			}
		}
		return h, nil

	case "i":
		return h, h.importSessions

	case "r":
		return h, h.loadSessions
	}

	return h, nil
}

// handleGroupDialogKey handles keys when group dialog is visible
func (h *Home) handleGroupDialogKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		switch h.groupDialog.Mode() {
		case GroupDialogCreate:
			name := h.groupDialog.GetValue()
			if name != "" {
				if h.groupDialog.HasParent() {
					// Create subgroup under parent
					parentPath := h.groupDialog.GetParentPath()
					h.groupTree.CreateSubgroup(parentPath, name)
				} else {
					// Create root-level group
					h.groupTree.CreateGroup(name)
				}
				h.rebuildFlatItems()
				h.saveInstances() // Persist the new group
			}
		case GroupDialogRename:
			name := h.groupDialog.GetValue()
			if name != "" {
				h.groupTree.RenameGroup(h.groupDialog.GetGroupPath(), name)
				h.instances = h.groupTree.GetAllInstances()
				h.rebuildFlatItems()
				h.saveInstances()
			}
		case GroupDialogMove:
			groupName := h.groupDialog.GetSelectedGroup()
			if groupName != "" && h.cursor < len(h.flatItems) {
				item := h.flatItems[h.cursor]
				if item.Type == session.ItemTypeSession {
					// Find the group path from name
					for _, g := range h.groupTree.GroupList {
						if g.Name == groupName {
							h.groupTree.MoveSessionToGroup(item.Session, g.Path)
							h.instances = h.groupTree.GetAllInstances()
							h.rebuildFlatItems()
							h.saveInstances()
							break
						}
					}
				}
			}
		case GroupDialogRenameSession:
			newName := h.groupDialog.GetValue()
			if newName != "" {
				sessionID := h.groupDialog.GetSessionID()
				// Find and rename the session
				for _, inst := range h.instances {
					if inst.ID == sessionID {
						inst.Title = newName
						break
					}
				}
				h.rebuildFlatItems()
				h.saveInstances()
			}
		}
		h.groupDialog.Hide()
		return h, nil
	case "esc":
		h.groupDialog.Hide()
		return h, nil
	}

	var cmd tea.Cmd
	h.groupDialog, cmd = h.groupDialog.Update(msg)
	return h, cmd
}

// saveInstances saves instances to storage
func (h *Home) saveInstances() {
	if h.storage != nil {
		// Save both instances and groups (including empty ones)
		h.storage.SaveWithGroups(h.instances, h.groupTree)
	}
}

// createSession creates a new session (deprecated, use createSessionInGroup)
func (h *Home) createSession(name, path, command string) tea.Cmd {
	return h.createSessionInGroup(name, path, command, "")
}

// createSessionInGroup creates a new session in a specific group
func (h *Home) createSessionInGroup(name, path, command, groupPath string) tea.Cmd {
	return func() tea.Msg {
		var inst *session.Instance
		if groupPath != "" {
			inst = session.NewInstanceWithGroup(name, path, groupPath)
		} else {
			inst = session.NewInstance(name, path)
		}
		inst.Command = command
		if err := inst.Start(); err != nil {
			return sessionCreatedMsg{err: err}
		}
		return sessionCreatedMsg{instance: inst}
	}
}

// sessionDeletedMsg signals that a session was deleted
type sessionDeletedMsg struct {
	deletedID string
}

// deleteSession deletes a session
func (h *Home) deleteSession(inst *session.Instance) tea.Cmd {
	id := inst.ID
	return func() tea.Msg {
		inst.Kill()
		return sessionDeletedMsg{deletedID: id}
	}
}

// attachSession attaches to a session using custom PTY with Ctrl+Q detection
func (h *Home) attachSession(inst *session.Instance) tea.Cmd {
	tmuxSess := inst.GetTmuxSession()
	if tmuxSess == nil {
		return nil
	}

	// Mark session as acknowledged (user is opening it)
	// This changes yellow (waiting) → gray (idle/seen) when they detach
	tmuxSess.Acknowledge()

	// Use tea.Exec with a custom command that runs our Attach method
	// On return, immediately update all session statuses (don't reload from storage
	// which would lose the tmux session state)
	return tea.Exec(attachCmd{session: tmuxSess}, func(err error) tea.Msg {
		// Baseline the content the user just saw to avoid a green flash on return
		tmuxSess.AcknowledgeWithSnapshot()
		return statusUpdateMsg{}
	})
}

// attachCmd implements tea.ExecCommand for custom PTY attach
type attachCmd struct {
	session *tmux.Session
}

func (a attachCmd) Run() error {
	ctx := context.Background()
	return a.session.Attach(ctx)
}

func (a attachCmd) SetStdin(r io.Reader)  {}
func (a attachCmd) SetStdout(w io.Writer) {}
func (a attachCmd) SetStderr(w io.Writer) {}

// importSessions imports existing tmux sessions
func (h *Home) importSessions() tea.Msg {
	discovered, err := session.DiscoverExistingTmuxSessions(h.instances)
	if err != nil {
		return loadSessionsMsg{err: err}
	}

	h.instances = append(h.instances, discovered...)
	if h.storage != nil {
		h.storage.Save(h.instances)
	}
	return loadSessionsMsg{instances: h.instances}
}

// countSessionStatuses counts sessions by status for the logo display
func (h *Home) countSessionStatuses() (running, waiting, idle int) {
	for _, inst := range h.instances {
		switch inst.Status {
		case session.StatusRunning:
			running++
		case session.StatusWaiting:
			waiting++
		case session.StatusIdle:
			idle++
		// StatusError is counted as neither - will show as idle in logo
		}
	}
	return running, waiting, idle
}

// updateSizes updates component sizes
func (h *Home) updateSizes() {
	h.search.SetSize(h.width, h.height)
	h.newDialog.SetSize(h.width, h.height)
	h.groupDialog.SetSize(h.width, h.height)
}

// View renders the UI
func (h *Home) View() string {
	if h.width == 0 {
		return "Loading..."
	}

	// Overlays take full screen
	if h.search.IsVisible() {
		return h.search.View()
	}
	if h.newDialog.IsVisible() {
		return h.newDialog.View()
	}
	if h.groupDialog.IsVisible() {
		return h.groupDialog.View()
	}

	var b strings.Builder

	// ═══════════════════════════════════════════════════════════════════
	// HEADER BAR
	// ═══════════════════════════════════════════════════════════════════
	// Calculate real session status counts for logo
	running, waiting, idle := h.countSessionStatuses()
	logo := RenderLogoCompact(running, waiting, idle)

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorAccent).
		Background(ColorSurface).
		Padding(0, 1)
	title := titleStyle.Render("Agent Deck")

	// Stats
	stats := lipgloss.NewStyle().Foreground(ColorTextDim).Render(
		fmt.Sprintf(" %d groups • %d sessions", h.groupTree.GroupCount(), h.groupTree.SessionCount()))

	// Fill remaining header space
	headerContent := lipgloss.JoinHorizontal(lipgloss.Left, logo, " ", title, stats)
	headerPadding := h.width - lipgloss.Width(headerContent)
	if headerPadding > 0 {
		headerContent += strings.Repeat(" ", headerPadding)
	}

	headerBar := lipgloss.NewStyle().
		Background(ColorSurface).
		Width(h.width).
		Render(headerContent)

	b.WriteString(headerBar)
	b.WriteString("\n")

	// ═══════════════════════════════════════════════════════════════════
	// MAIN CONTENT AREA
	// ═══════════════════════════════════════════════════════════════════
	helpBarHeight := 3 // Help bar takes 3 lines
	contentHeight := h.height - 2 - helpBarHeight // -2 for header, -helpBarHeight for help

	// Calculate panel widths (35% left, 65% right for more preview space)
	leftWidth := int(float64(h.width) * 0.35)
	rightWidth := h.width - leftWidth - 3 // -3 for separator

	// Build left panel (session list) with title
	leftTitle := lipgloss.NewStyle().
		Foreground(ColorCyan).
		Bold(true).
		Render("SESSIONS")
	leftContent := h.renderSessionList(contentHeight - 2) // -2 for title
	leftPanel := lipgloss.JoinVertical(lipgloss.Left, leftTitle, leftContent)
	leftPanel = lipgloss.NewStyle().
		Width(leftWidth).
		Height(contentHeight).
		Render(leftPanel)

	// Build right panel (preview) with title
	rightTitle := lipgloss.NewStyle().
		Foreground(ColorCyan).
		Bold(true).
		Render("PREVIEW")
	rightContent := h.renderPreviewPane(rightWidth, contentHeight-2) // -2 for title
	rightPanel := lipgloss.JoinVertical(lipgloss.Left, rightTitle, rightContent)
	rightPanel = lipgloss.NewStyle().
		Width(rightWidth).
		Height(contentHeight).
		Render(rightPanel)

	// Build separator
	separatorStyle := lipgloss.NewStyle().Foreground(ColorBorder)
	separatorLines := make([]string, contentHeight)
	for i := range separatorLines {
		separatorLines[i] = separatorStyle.Render(" │ ")
	}
	separator := strings.Join(separatorLines, "\n")

	// Join panels horizontally
	mainContent := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, separator, rightPanel)
	b.WriteString(mainContent)
	b.WriteString("\n")

	// ═══════════════════════════════════════════════════════════════════
	// HELP BAR (context-aware shortcuts)
	// ═══════════════════════════════════════════════════════════════════
	helpBar := h.renderHelpBar()
	b.WriteString(helpBar)

	// Error display (above help bar)
	if h.err != nil {
		errMsg := ErrorStyle.Render("⚠ " + h.err.Error())
		b.WriteString("\n")
		b.WriteString(errMsg)
	}

	return b.String()
}

// renderHelpBar renders context-aware keyboard shortcuts
func (h *Home) renderHelpBar() string {
	// Determine context
	var contextHints []string
	var contextTitle string

	if len(h.flatItems) == 0 {
		contextTitle = "No sessions"
		contextHints = []string{
			h.helpKey("n", "New session"),
			h.helpKey("i", "Import tmux"),
			h.helpKey("g", "New group"),
			h.helpKey("q", "Quit"),
		}
	} else if h.cursor < len(h.flatItems) {
		item := h.flatItems[h.cursor]
		if item.Type == session.ItemTypeGroup {
			contextTitle = "Group selected"
			contextHints = []string{
				h.helpKey("Tab/l", "Toggle"),
				h.helpKey("R", "Rename"),
				h.helpKey("d", "Delete"),
				h.helpKey("g", "New subgroup"),
				h.helpKey("n", "New session"),
			}
		} else {
			contextTitle = "Session selected"
			contextHints = []string{
				h.helpKey("Enter", "Attach"),
				h.helpKey("R", "Rename"),
				h.helpKey("m", "Move to group"),
				h.helpKey("d", "Delete"),
				h.helpKey("h/←", "Collapse group"),
			}
		}
	}

	// Build help bar
	border := lipgloss.NewStyle().Foreground(ColorBorder).Render(strings.Repeat("─", h.width))

	// Context title
	ctxStyle := lipgloss.NewStyle().Foreground(ColorPurple).Bold(true)

	// Build shortcuts line
	shortcutsLine := strings.Join(contextHints, "  ")

	// Global shortcuts (always shown)
	globalHints := lipgloss.NewStyle().Foreground(ColorTextDim).Render(
		"  │  ↑↓/jk Navigate  /Search  Ctrl+Q Detach  q Quit")

	helpContent := lipgloss.JoinHorizontal(lipgloss.Left,
		ctxStyle.Render(contextTitle+": "),
		shortcutsLine,
		globalHints,
	)

	return lipgloss.JoinVertical(lipgloss.Left, border, helpContent)
}

// helpKey formats a keyboard shortcut for the help bar
func (h *Home) helpKey(key, desc string) string {
	keyStyle := lipgloss.NewStyle().
		Foreground(ColorBg).
		Background(ColorAccent).
		Bold(true).
		Padding(0, 1)
	descStyle := lipgloss.NewStyle().Foreground(ColorText)
	return keyStyle.Render(key) + descStyle.Render(" "+desc)
}

// renderSessionList renders the left panel with hierarchical session list
func (h *Home) renderSessionList(height int) string {
	var b strings.Builder

	if len(h.flatItems) == 0 {
		// Large logo for empty state - shows real status (all idle when no sessions)
		running, waiting, idle := h.countSessionStatuses()
		largeLogo := RenderLogoLarge(running, waiting, idle)

		// App title
		titleStyle := lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorAccent)
		appTitle := titleStyle.Render("Agent Deck")

		// Subtitle
		subtitleStyle := lipgloss.NewStyle().
			Foreground(ColorTextDim).
			Italic(true)
		subtitle := subtitleStyle.Render("Terminal Session Manager")

		// Instructions
		instructions := lipgloss.NewStyle().Foreground(ColorAccent).Render("n") + " Create new\n" +
			lipgloss.NewStyle().Foreground(ColorAccent).Render("i") + " Import from tmux\n" +
			lipgloss.NewStyle().Foreground(ColorAccent).Render("g") + " Create group"

		// Combine all elements
		content := lipgloss.JoinVertical(lipgloss.Center,
			largeLogo,
			"",
			appTitle,
			subtitle,
			"",
			instructions,
		)

		emptyBox := lipgloss.NewStyle().
			Foreground(ColorTextDim).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorBorder).
			Padding(1, 3).
			Render(content)
		return emptyBox
	}

	// Render items starting from viewOffset
	visibleCount := 0
	maxVisible := height - 1 // Leave room for scrolling indicator
	if maxVisible < 1 {
		maxVisible = 1
	}

	// Show "more above" indicator if scrolled down
	if h.viewOffset > 0 {
		b.WriteString(DimStyle.Render(fmt.Sprintf("  ⋮ +%d above", h.viewOffset)))
		b.WriteString("\n")
		maxVisible-- // Account for the indicator line
	}

	for i := h.viewOffset; i < len(h.flatItems) && visibleCount < maxVisible; i++ {
		item := h.flatItems[i]
		h.renderItem(&b, item, i == h.cursor)
		visibleCount++
	}

	// Show "more below" indicator if there are more items
	remaining := len(h.flatItems) - (h.viewOffset + visibleCount)
	if remaining > 0 {
		b.WriteString(DimStyle.Render(fmt.Sprintf("  ⋮ +%d below", remaining)))
	}

	return b.String()
}

// renderItem renders a single item (group or session) for the left panel
func (h *Home) renderItem(b *strings.Builder, item session.Item, selected bool) {
	if item.Type == session.ItemTypeGroup {
		h.renderGroupItem(b, item, selected)
	} else {
		h.renderSessionItem(b, item, selected)
	}
}

// renderGroupItem renders a group header
func (h *Home) renderGroupItem(b *strings.Builder, item session.Item, selected bool) {
	group := item.Group

	// Calculate indentation based on nesting level
	indent := strings.Repeat("  ", item.Level)
	if item.Level > 0 {
		indent = strings.Repeat("  ", item.Level-1) + "  ├─ "
	}

	// Expand/collapse indicator
	expandIcon := lipgloss.NewStyle().Foreground(ColorTextDim).Render("▼")
	if !group.Expanded {
		expandIcon = lipgloss.NewStyle().Foreground(ColorTextDim).Render("▶")
	}

	// Group name with count
	nameStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	countStyle := lipgloss.NewStyle().Foreground(ColorTextDim)

	if selected {
		nameStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorBg).
			Background(ColorAccent)
		countStyle = lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorAccent)
		expandIcon = lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorAccent).
			Render("▶")
		if group.Expanded {
			expandIcon = lipgloss.NewStyle().
				Foreground(ColorBg).
				Background(ColorAccent).
				Render("▼")
		}
	}

	sessionCount := len(group.Sessions)
	countStr := countStyle.Render(fmt.Sprintf(" (%d)", sessionCount))

	// Check if any session in group is running
	running := 0
	waiting := 0
	for _, sess := range group.Sessions {
		switch sess.Status {
		case session.StatusRunning:
			running++
		case session.StatusWaiting:
			waiting++
		}
	}

	// Status indicators
	statusStr := ""
	if running > 0 {
		statusStr += lipgloss.NewStyle().Foreground(ColorGreen).Render(fmt.Sprintf(" ●%d", running))
	}
	if waiting > 0 {
		statusStr += lipgloss.NewStyle().Foreground(ColorYellow).Render(fmt.Sprintf(" ◐%d", waiting))
	}

	// Build the row with proper indentation
	row := fmt.Sprintf("%s%s %s%s%s", indent, expandIcon, nameStyle.Render(group.Name), countStr, statusStr)
	b.WriteString(row)
	b.WriteString("\n")
}

// renderSessionItem renders a single session item for the left panel
func (h *Home) renderSessionItem(b *strings.Builder, item session.Item, selected bool) {
	inst := item.Session

	// Calculate indentation based on nesting level
	// Sessions are always under a group, so Level >= 1
	indent := strings.Repeat("  ", item.Level-1)
	treeLine := indent + "  ├─ "
	if selected {
		treeLine = indent + "  "
	}

	// Status indicator
	var statusIcon string
	var statusColor lipgloss.Color
	switch inst.Status {
	case session.StatusRunning:
		statusIcon = "●"
		statusColor = ColorGreen
	case session.StatusWaiting:
		statusIcon = "◐"
		statusColor = ColorYellow
	case session.StatusIdle:
		statusIcon = "○"
		statusColor = ColorTextDim
	case session.StatusError:
		statusIcon = "✕"
		statusColor = ColorRed
	default:
		statusIcon = "?"
		statusColor = ColorTextDim
	}

	status := lipgloss.NewStyle().Foreground(statusColor).Render(statusIcon)

	// Title and tool
	titleStyle := lipgloss.NewStyle().Foreground(ColorText)
	toolStyle := lipgloss.NewStyle().Foreground(ColorPurple)

	if selected {
		titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorBg).
			Background(ColorAccent)
		toolStyle = lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorAccent)
		// Override tree line styling when selected
		treeLine = lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorAccent).
			Render(indent + "▶ ")
	}

	title := titleStyle.Render(inst.Title)
	tool := toolStyle.Render(fmt.Sprintf(" [%s]", inst.Tool))

	// Build row
	treeStyle := lipgloss.NewStyle().Foreground(ColorBorder)
	if !selected {
		treeLine = treeStyle.Render(treeLine)
	}

	row := fmt.Sprintf("%s%s %s%s", treeLine, status, title, tool)
	b.WriteString(row)
	b.WriteString("\n")
}

// renderPreviewPane renders the right panel with live preview
func (h *Home) renderPreviewPane(width, height int) string {
	var b strings.Builder

	if len(h.flatItems) == 0 || h.cursor >= len(h.flatItems) {
		emptyBox := lipgloss.NewStyle().
			Foreground(ColorTextDim).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorBorder).
			Padding(1, 2).
			Render("Select a session to see its terminal output")
		return emptyBox
	}

	item := h.flatItems[h.cursor]

	// If group is selected, show group info
	if item.Type == session.ItemTypeGroup {
		return h.renderGroupPreview(item.Group, width, height)
	}

	// Session preview
	selected := item.Session

	// Session info header box
	statusIcon := "○"
	statusColor := ColorTextDim
	switch selected.Status {
	case session.StatusRunning:
		statusIcon = "●"
		statusColor = ColorGreen
	case session.StatusWaiting:
		statusIcon = "◐"
		statusColor = ColorYellow
	case session.StatusError:
		statusIcon = "✕"
		statusColor = ColorRed
	}

	// Header with session name and status
	statusBadge := lipgloss.NewStyle().Foreground(statusColor).Render(statusIcon + " " + string(selected.Status))
	nameStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorAccent)
	b.WriteString(nameStyle.Render(selected.Title))
	b.WriteString("  ")
	b.WriteString(statusBadge)
	b.WriteString("\n")

	// Info line
	infoStyle := lipgloss.NewStyle().Foreground(ColorTextDim)
	pathStr := truncatePath(selected.ProjectPath, width-4)
	b.WriteString(infoStyle.Render("📁 " + pathStr))
	b.WriteString("\n")

	toolBadge := lipgloss.NewStyle().
		Foreground(ColorBg).
		Background(ColorPurple).
		Padding(0, 1).
		Render(selected.Tool)
	groupBadge := lipgloss.NewStyle().
		Foreground(ColorBg).
		Background(ColorCyan).
		Padding(0, 1).
		Render(selected.GroupPath)
	b.WriteString(toolBadge)
	b.WriteString(" ")
	b.WriteString(groupBadge)
	b.WriteString("\n\n")

	// Terminal output header
	termHeader := lipgloss.NewStyle().
		Foreground(ColorTextDim).
		Render("─── Terminal Output ───")
	b.WriteString(termHeader)
	b.WriteString("\n")

	// Terminal preview
	preview, err := selected.PreviewFull()
	if err != nil {
		b.WriteString(ErrorStyle.Render("⚠ " + err.Error()))
	} else if preview == "" {
		emptyTerm := lipgloss.NewStyle().
			Foreground(ColorTextDim).
			Italic(true).
			Render("(terminal is empty)")
		b.WriteString(emptyTerm)
	} else {
		// Limit preview to available height
		lines := strings.Split(preview, "\n")
		maxLines := height - 8 // Account for header and info
		if maxLines < 1 {
			maxLines = 1
		}
		if len(lines) > maxLines {
			lines = lines[len(lines)-maxLines:]
		}

		previewStyle := lipgloss.NewStyle().Foreground(ColorText)
		maxWidth := width - 4
		if maxWidth < 10 {
			maxWidth = 10
		}

		for _, line := range lines {
			// Strip ANSI codes for accurate length measurement
			cleanLine := ansiRegex.ReplaceAllString(line, "")

			// Skip completely empty lines to reduce noise
			trimmed := strings.TrimSpace(cleanLine)
			if trimmed == "" {
				continue
			}

			// Truncate based on visible character length (runes, not bytes)
			runes := []rune(cleanLine)
			if len(runes) > maxWidth {
				cleanLine = string(runes[:maxWidth-3]) + "..."
			}

			b.WriteString(previewStyle.Render(cleanLine))
			b.WriteString("\n")
		}
	}

	return b.String()
}

// truncatePath shortens a path to fit within maxLen characters
func truncatePath(path string, maxLen int) string {
	if len(path) <= maxLen {
		return path
	}
	if maxLen < 10 {
		maxLen = 10
	}
	// Show beginning and end: /Users/.../project
	return path[:maxLen/3] + "..." + path[len(path)-(maxLen*2/3-3):]
}

// renderGroupPreview renders the preview pane for a group
func (h *Home) renderGroupPreview(group *session.Group, width, height int) string {
	var b strings.Builder

	// Group name header
	nameStyle := lipgloss.NewStyle().Bold(true).Foreground(ColorCyan)
	b.WriteString(nameStyle.Render("📁 " + group.Name))
	b.WriteString("\n\n")

	// Group stats
	running := 0
	waiting := 0
	idle := 0
	errored := 0
	for _, sess := range group.Sessions {
		switch sess.Status {
		case session.StatusRunning:
			running++
		case session.StatusWaiting:
			waiting++
		case session.StatusIdle:
			idle++
		case session.StatusError:
			errored++
		}
	}

	// Stats in a nice box format
	totalBadge := lipgloss.NewStyle().
		Foreground(ColorText).
		Bold(true).
		Render(fmt.Sprintf("%d sessions", len(group.Sessions)))
	b.WriteString(totalBadge)
	b.WriteString("\n")

	// Status breakdown with badges
	if running > 0 {
		badge := lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorGreen).
			Padding(0, 1).
			Render(fmt.Sprintf("● %d running", running))
		b.WriteString(badge)
		b.WriteString(" ")
	}
	if waiting > 0 {
		badge := lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorYellow).
			Padding(0, 1).
			Render(fmt.Sprintf("◐ %d waiting", waiting))
		b.WriteString(badge)
		b.WriteString(" ")
	}
	if idle > 0 {
		badge := lipgloss.NewStyle().
			Foreground(ColorText).
			Background(ColorBorder).
			Padding(0, 1).
			Render(fmt.Sprintf("○ %d idle", idle))
		b.WriteString(badge)
		b.WriteString(" ")
	}
	if errored > 0 {
		badge := lipgloss.NewStyle().
			Foreground(ColorBg).
			Background(ColorRed).
			Padding(0, 1).
			Render(fmt.Sprintf("✕ %d error", errored))
		b.WriteString(badge)
	}
	b.WriteString("\n\n")

	// Session list header
	listHeader := lipgloss.NewStyle().
		Foreground(ColorTextDim).
		Render("─── Sessions ───")
	b.WriteString(listHeader)
	b.WriteString("\n")

	// Session list
	for i, sess := range group.Sessions {
		if i >= height-10 { // Leave room
			b.WriteString(DimStyle.Render(fmt.Sprintf("  ⋮ +%d more", len(group.Sessions)-i)))
			break
		}
		statusIcon := "○"
		statusColor := ColorTextDim
		switch sess.Status {
		case session.StatusRunning:
			statusIcon = "●"
			statusColor = ColorGreen
		case session.StatusWaiting:
			statusIcon = "◐"
			statusColor = ColorYellow
		case session.StatusError:
			statusIcon = "✕"
			statusColor = ColorRed
		}
		status := lipgloss.NewStyle().Foreground(statusColor).Render(statusIcon)
		name := lipgloss.NewStyle().Foreground(ColorText).Render(sess.Title)
		tool := lipgloss.NewStyle().Foreground(ColorPurple).Render(fmt.Sprintf("[%s]", sess.Tool))
		b.WriteString(fmt.Sprintf("  %s %s %s\n", status, name, tool))
	}

	// Help hint at bottom
	b.WriteString("\n")
	hintStyle := lipgloss.NewStyle().Foreground(ColorTextDim).Italic(true)
	b.WriteString(hintStyle.Render("Tab: expand/collapse • G: rename • d: delete"))

	return b.String()
}
