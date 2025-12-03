package tmux

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Debug flag - set via environment variable AGENTDECK_DEBUG=1
var debugStatusEnabled = os.Getenv("AGENTDECK_DEBUG") == "1"

func debugLog(format string, args ...interface{}) {
	if debugStatusEnabled {
		log.Printf("[STATUS] "+format, args...)
	}
}

const SessionPrefix = "agentdeck_"

// Tool detection patterns (used by DetectTool for initial tool identification)
var toolDetectionPatterns = map[string][]*regexp.Regexp{
	"claude": {
		regexp.MustCompile(`(?i)claude`),
		regexp.MustCompile(`(?i)anthropic`),
	},
	"gemini": {
		regexp.MustCompile(`(?i)gemini`),
		regexp.MustCompile(`(?i)google ai`),
	},
	"aider": {
		regexp.MustCompile(`(?i)aider`),
	},
	"codex": {
		regexp.MustCompile(`(?i)codex`),
		regexp.MustCompile(`(?i)openai`),
	},
}

// StateTracker tracks content changes for notification-style status detection
//
// Time-based model to prevent flickering:
//   GREEN (active)   = Content changed recently (within activityCooldown) AND stabilized
//   YELLOW (waiting) = No changes for activityCooldown + NOT acknowledged
//   GRAY (idle)      = No changes for activityCooldown + acknowledged
//
// Key insight: AI agents output in bursts with micro-pauses between them.
// Using a time-based cooldown instead of consecutive-change counting prevents
// flickering during these natural pauses in output.
//
// The lastChangeTime tracks when content last changed:
// - Updated whenever hash changes
// - GREEN shown while time.Since(lastChangeTime) < activityCooldown
// - YELLOW/GRAY shown after cooldown expires
//
// The stabilized flag prevents green flash on new sessions:
// - New sessions need to "settle" before showing activity
// - Set to true after first stable poll (content unchanged for activityCooldown)
// - Only show GREEN after stabilization
type StateTracker struct {
	lastHash       string    // SHA256 of last captured content
	lastChangeTime time.Time // When content last changed
	acknowledged   bool      // User has seen this "stopped" state
	stabilized     bool      // Session has had at least one stable poll (prevents green flash on new sessions)
	lastContent    string    // Debug: previous normalized content for diff
}

// activityCooldown is how long to show GREEN after content stops changing.
// This prevents flickering during natural micro-pauses in AI output.
// - 2 seconds: Covers most pauses between output bursts
// - 3 seconds: More conservative, fewer false yellows
const activityCooldown = 2 * time.Second

// Session represents a tmux session
type Session struct {
	Name        string
	DisplayName string
	WorkDir     string
	Command     string
	Created     time.Time
	lastHash    string
	lastContent string
	// Cached tool detection (avoids re-detecting every status check)
	detectedTool     string
	toolDetectedAt   time.Time
	toolDetectExpiry time.Duration // How long before re-detecting (default 30s)
	// Simple state tracking (hash-based)
	stateTracker   *StateTracker
	stateTrackerMu sync.Mutex // Protects stateTracker from concurrent access
	// Last status returned (for debugging)
	lastStableStatus string
	// Prompt detection (for tool-specific prompts)
	promptDetector *PromptDetector
}

// ensureStateTrackerLocked lazily allocates the tracker so callers can safely
// acknowledge even before the first GetStatus call.
// MUST be called with stateTrackerMu held.
func (s *Session) ensureStateTrackerLocked() {
	if s.stateTracker == nil {
		s.stateTracker = &StateTracker{
			lastHash: "",
			// Set lastChangeTime in the past so cooldown is already expired
			// This is consistent with GetStatus initialization
			lastChangeTime: time.Now().Add(-activityCooldown),
			acknowledged:   false,
			stabilized:     false, // Will be set true after first stable poll
		}
	}
}

// NewSession creates a new Session instance with a unique name
func NewSession(name, workDir string) *Session {
	sanitized := sanitizeName(name)
	// Add unique suffix to prevent name collisions
	uniqueSuffix := generateShortID()
	return &Session{
		Name:             SessionPrefix + sanitized + "_" + uniqueSuffix,
		DisplayName:      name,
		WorkDir:          workDir,
		Created:          time.Now(),
		lastStableStatus: "waiting",
		toolDetectExpiry: 30 * time.Second, // Re-detect tool every 30 seconds
		// stateTracker and promptDetector will be created lazily on first status check
	}
}

// ReconnectSession creates a Session object for an existing tmux session
// This is used when loading sessions from storage - it properly initializes
// all fields needed for status detection to work correctly
func ReconnectSession(tmuxName, displayName, workDir, command string) *Session {
	return &Session{
		Name:             tmuxName,
		DisplayName:      displayName,
		WorkDir:          workDir,
		Command:          command,
		Created:          time.Now(), // Approximate - we don't persist this
		lastStableStatus: "waiting",
		toolDetectExpiry: 30 * time.Second,
		// stateTracker and promptDetector will be created lazily on first status check
	}
}

// ReconnectSessionWithStatus creates a Session with pre-initialized state based on previous status
// This restores the exact status state across app restarts:
//   - "idle" (gray): acknowledged=true, cooldown expired
//   - "waiting" (yellow): acknowledged=false, cooldown expired
//   - "active" (green): will be recalculated based on actual content changes
func ReconnectSessionWithStatus(tmuxName, displayName, workDir, command string, previousStatus string) *Session {
	sess := ReconnectSession(tmuxName, displayName, workDir, command)

	switch previousStatus {
	case "idle":
		// Session was acknowledged (user saw it) - restore as GRAY
		sess.stateTracker = &StateTracker{
			lastHash:       "",                                // Will be set on first GetStatus
			lastChangeTime: time.Now().Add(-10 * time.Second), // Cooldown expired
			acknowledged:   true,
			stabilized:     true,
		}
		sess.lastStableStatus = "idle"

	case "waiting":
		// Session was waiting for attention - restore as YELLOW
		sess.stateTracker = &StateTracker{
			lastHash:       "",                                // Will be set on first GetStatus
			lastChangeTime: time.Now().Add(-10 * time.Second), // Cooldown expired
			acknowledged:   false,                             // NOT acknowledged - still needs attention
			stabilized:     true,
		}
		sess.lastStableStatus = "waiting"

	case "active":
		// Session was active - let it be recalculated from actual content
		// Don't pre-initialize tracker, let GetStatus handle it
		sess.lastStableStatus = "waiting" // Default to waiting until we see activity

	default:
		// Unknown status - default to waiting
		sess.lastStableStatus = "waiting"
	}

	return sess
}

// generateShortID generates a short random ID for uniqueness
func generateShortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp
		return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	}
	return hex.EncodeToString(b)
}

// sanitizeName converts a display name to a valid tmux session name
func sanitizeName(name string) string {
	// Replace spaces and special characters with hyphens
	re := regexp.MustCompile(`[^a-zA-Z0-9-]+`)
	return re.ReplaceAllString(name, "-")
}

// Start creates and starts a tmux session
func (s *Session) Start(command string) error {
	s.Command = command

	// Check if session already exists (shouldn't happen with unique IDs, but handle gracefully)
	if s.Exists() {
		// Session with this exact name exists - regenerate with new unique suffix
		sanitized := sanitizeName(s.DisplayName)
		s.Name = SessionPrefix + sanitized + "_" + generateShortID()
	}

	// Ensure working directory exists
	workDir := s.WorkDir
	if workDir == "" {
		workDir = os.Getenv("HOME")
	}

	// Create new tmux session in detached mode
	cmd := exec.Command("tmux", "new-session", "-d", "-s", s.Name, "-c", workDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create tmux session: %w (output: %s)", err, string(output))
	}

	// Enable mouse mode for proper scrolling (per-session, doesn't affect user's other sessions)
	// This allows:
	// - Mouse wheel scrolling through terminal history
	// - Text selection with mouse
	// - Pane resizing with mouse
	mouseCmd := exec.Command("tmux", "set-option", "-t", s.Name, "mouse", "on")
	if err := mouseCmd.Run(); err != nil {
		// Non-fatal: session still works, just without mouse support
		// This can fail on very old tmux versions
	}

	// Send the command to the session
	if command != "" {
		if err := s.SendKeys(command); err != nil {
			return fmt.Errorf("failed to send command: %w", err)
		}
		if err := s.SendEnter(); err != nil {
			return fmt.Errorf("failed to send enter: %w", err)
		}
	}

	return nil
}

// Exists checks if the tmux session exists
func (s *Session) Exists() bool {
	cmd := exec.Command("tmux", "has-session", "-t", s.Name)
	return cmd.Run() == nil
}

// EnableMouseMode enables mouse scrolling for this session
// Safe to call multiple times - just sets the option again
func (s *Session) EnableMouseMode() error {
	cmd := exec.Command("tmux", "set-option", "-t", s.Name, "mouse", "on")
	return cmd.Run()
}

// Kill terminates the tmux session
func (s *Session) Kill() error {
	cmd := exec.Command("tmux", "kill-session", "-t", s.Name)
	return cmd.Run()
}

// CapturePane captures the visible pane content
func (s *Session) CapturePane() (string, error) {
	// -J joins wrapped lines and trims trailing spaces so hashes don't change on resize
	cmd := exec.Command("tmux", "capture-pane", "-t", s.Name, "-p", "-J")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to capture pane: %w", err)
	}
	return string(output), nil
}

// CaptureFullHistory captures the scrollback history (limited to last 500 lines for performance)
func (s *Session) CaptureFullHistory() (string, error) {
	// Limit to last 500 lines to prevent memory issues with long-running sessions
	// -J joins wrapped lines and trims trailing spaces so hashes don't change on resize
	cmd := exec.Command("tmux", "capture-pane", "-t", s.Name, "-p", "-J", "-S", "-500")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to capture history: %w", err)
	}
	return string(output), nil
}

// HasUpdated checks if the pane content has changed since last check
func (s *Session) HasUpdated() (bool, error) {
	content, err := s.CapturePane()
	if err != nil {
		return false, err
	}

	// Calculate SHA256 hash of content
	hash := sha256.Sum256([]byte(content))
	hashStr := hex.EncodeToString(hash[:])

	// First time check
	if s.lastHash == "" {
		s.lastHash = hashStr
		s.lastContent = content
		return true, nil
	}

	// Compare with previous hash
	if hashStr != s.lastHash {
		s.lastHash = hashStr
		s.lastContent = content
		return true, nil
	}

	return false, nil
}

// DetectTool detects which AI coding tool is running in the session
// Uses caching to avoid re-detection on every call
func (s *Session) DetectTool() string {
	// Return cached result if still valid
	if s.detectedTool != "" && time.Since(s.toolDetectedAt) < s.toolDetectExpiry {
		return s.detectedTool
	}

	// Detect tool from command first (most reliable)
	if s.Command != "" {
		cmdLower := strings.ToLower(s.Command)
		if strings.Contains(cmdLower, "claude") {
			s.detectedTool = "claude"
			s.toolDetectedAt = time.Now()
			return s.detectedTool
		}
		if strings.Contains(cmdLower, "gemini") {
			s.detectedTool = "gemini"
			s.toolDetectedAt = time.Now()
			return s.detectedTool
		}
		if strings.Contains(cmdLower, "aider") {
			s.detectedTool = "aider"
			s.toolDetectedAt = time.Now()
			return s.detectedTool
		}
		if strings.Contains(cmdLower, "codex") {
			s.detectedTool = "codex"
			s.toolDetectedAt = time.Now()
			return s.detectedTool
		}
	}

	// Fallback to content detection
	content, err := s.CapturePane()
	if err != nil {
		s.detectedTool = "shell"
		s.toolDetectedAt = time.Now()
		return s.detectedTool
	}

	// Strip ANSI codes for accurate matching
	cleanContent := StripANSI(content)

	// Check using pre-compiled patterns
	for tool, patterns := range toolDetectionPatterns {
		for _, pattern := range patterns {
			if pattern.MatchString(cleanContent) {
				s.detectedTool = tool
				s.toolDetectedAt = time.Now()
				return s.detectedTool
			}
		}
	}

	s.detectedTool = "shell"
	s.toolDetectedAt = time.Now()
	return s.detectedTool
}

// ForceDetectTool forces a re-detection of the tool, ignoring cache
func (s *Session) ForceDetectTool() string {
	s.detectedTool = ""
	s.toolDetectedAt = time.Time{}
	return s.DetectTool()
}

// AcknowledgeWithSnapshot marks the session as seen and baselines the current
// content hash. Called when user detaches from session.
func (s *Session) AcknowledgeWithSnapshot() {
	shortName := s.DisplayName
	if len(shortName) > 12 {
		shortName = shortName[:12]
	}

	// Capture content before acquiring lock (CapturePane is slow)
	var content string
	var captureErr error
	exists := s.Exists()
	if exists {
		content, captureErr = s.CapturePane()
	}

	s.stateTrackerMu.Lock()
	defer s.stateTrackerMu.Unlock()

	s.ensureStateTrackerLocked()

	if !exists {
		s.stateTracker.acknowledged = true
		s.lastStableStatus = "inactive"
		debugLog("%s: AckSnapshot session gone → inactive", shortName)
		return
	}

	if captureErr != nil {
		s.stateTracker.acknowledged = true
		s.lastStableStatus = "idle"
		debugLog("%s: AckSnapshot capture error → idle", shortName)
		return
	}

	// Snapshot current content so next poll doesn't trigger "active"
	cleanContent := s.normalizeContent(content)
	newHash := s.hashContent(cleanContent)
	prevHash := s.stateTracker.lastHash
	s.stateTracker.lastHash = newHash
	s.stateTracker.acknowledged = true
	s.lastStableStatus = "idle"
	prevHashShort := "(empty)"
	if len(prevHash) >= 16 {
		prevHashShort = prevHash[:16]
	}
	debugLog("%s: AckSnapshot hash %s → %s, ack=true → idle", shortName, prevHashShort, newHash[:16])
}

// GetStatus returns the current status of the session
//
// Time-based 3-state model to prevent flickering:
//   GREEN (active)   = Content changed within activityCooldown (2 seconds)
//   YELLOW (waiting) = Cooldown expired + NOT acknowledged (needs attention)
//   GRAY (idle)      = Cooldown expired + acknowledged (user has seen it)
//
// Key insight: AI agents output in bursts with micro-pauses. A time-based
// cooldown prevents flickering during these natural pauses - we stay GREEN
// for 2 seconds after ANY content change, regardless of micro-pauses.
//
// Logic:
// 1. Capture content and hash it
// 2. If hash changed → update lastChangeTime, return GREEN
// 3. If hash same → check if cooldown expired
//    - If within cooldown → GREEN (still considered active)
//    - If cooldown expired → YELLOW or GRAY based on acknowledged
func (s *Session) GetStatus() (string, error) {
	shortName := s.DisplayName
	if len(shortName) > 12 {
		shortName = shortName[:12]
	}

	// Perform expensive operations before acquiring lock
	if !s.Exists() {
		s.stateTrackerMu.Lock()
		s.lastStableStatus = "inactive"
		s.stateTrackerMu.Unlock()
		debugLog("%s: session doesn't exist → inactive", shortName)
		return "inactive", nil
	}

	// Capture current content (slow operation - do before lock)
	content, err := s.CapturePane()
	if err != nil {
		s.stateTrackerMu.Lock()
		s.lastStableStatus = "inactive"
		s.stateTrackerMu.Unlock()
		debugLog("%s: capture error → inactive", shortName)
		return "inactive", nil
	}

	// Clean content: strip ANSI codes, spinner characters, normalize whitespace
	cleanContent := s.normalizeContent(content)
	currentHash := s.hashContent(cleanContent)

	// Handle empty content - use placeholder hash to avoid edge cases
	if currentHash == "" || cleanContent == "" {
		currentHash = "__empty__"
	}

	// Now acquire lock for state manipulation
	s.stateTrackerMu.Lock()
	defer s.stateTrackerMu.Unlock()

	// Initialize state tracker on first call
	if s.stateTracker == nil {
		s.stateTracker = &StateTracker{
			lastHash:       currentHash,
			// Set lastChangeTime in the past so cooldown is already expired.
			// This prevents YELLOW → GREEN flickering on session initialization.
			// Without this, the cooldown would trigger false "active" for 2 seconds.
			lastChangeTime: time.Now().Add(-activityCooldown),
			acknowledged:   false,
			stabilized:     false, // New sessions must stabilize before showing green
		}
		// First poll: return "waiting" (session needs attention until user sees it)
		s.lastStableStatus = "waiting"
		debugLog("%s: INIT tracker hash=%s, stabilized=false → waiting", shortName, currentHash[:16])
		return "waiting", nil
	}

	// Restored session with empty hash - initialize without triggering "active"
	if s.stateTracker.lastHash == "" {
		s.stateTracker.lastHash = currentHash
		if s.stateTracker.acknowledged {
			s.lastStableStatus = "idle"
			debugLog("%s: empty hash restored, ack=true → idle", shortName)
			return "idle", nil
		}
		s.lastStableStatus = "waiting"
		debugLog("%s: empty hash restored, ack=false → waiting", shortName)
		return "waiting", nil
	}

	// Check if content changed
	if s.stateTracker.lastHash != currentHash {
		// Content CHANGED - update hash and time, return GREEN
		prevHash := s.stateTracker.lastHash

		// Debug: Show what changed between polls
		if debugStatusEnabled {
			debugLog("%s: CONTENT CHANGED %s → %s", shortName, prevHash[:16], currentHash[:16])

			// Show last 5 lines of new content
			newLines := strings.Split(cleanContent, "\n")
			start := len(newLines) - 5
			if start < 0 {
				start = 0
			}
			newLast := strings.Join(newLines[start:], " | ")
			if len(newLast) > 200 {
				newLast = newLast[:200] + "..."
			}
			debugLog("%s: NEW CONTENT (last 5 lines): %s", shortName, newLast)

			// Show last 5 lines of previous content for comparison
			if s.stateTracker.lastContent != "" {
				oldLines := strings.Split(s.stateTracker.lastContent, "\n")
				start := len(oldLines) - 5
				if start < 0 {
					start = 0
				}
				oldLast := strings.Join(oldLines[start:], " | ")
				if len(oldLast) > 200 {
					oldLast = oldLast[:200] + "..."
				}
				debugLog("%s: OLD CONTENT (last 5 lines): %s", shortName, oldLast)
			}
		}

		s.stateTracker.lastHash = currentHash
		s.stateTracker.lastContent = cleanContent // Store for next diff
		s.stateTracker.lastChangeTime = time.Now()
		s.stateTracker.acknowledged = false // Reset for next notification

		// Only show GREEN if the session has stabilized (had at least one stable poll)
		// This prevents green flash when new sessions are initializing
		if s.stateTracker.stabilized {
			s.lastStableStatus = "active"
			debugLog("%s: CONTENT CHANGED, stabilized=true → active", shortName)
			return "active", nil
		}

		// Not yet stabilized - content is changing during initialization
		// Keep showing waiting (yellow) until session settles
		s.lastStableStatus = "waiting"
		debugLog("%s: CONTENT CHANGED, stabilized=false → waiting (initialization)", shortName)
		return "waiting", nil
	}

	// Content STABLE - check if we're still within the cooldown period
	timeSinceChange := time.Since(s.stateTracker.lastChangeTime)

	if timeSinceChange < activityCooldown {
		// Still within cooldown
		// Only show GREEN if already stabilized (previous output was active)
		if s.stateTracker.stabilized {
			s.lastStableStatus = "active"
			debugLog("%s: COOLDOWN ACTIVE (%.1fs < %.1fs), stabilized=true → active",
				shortName, timeSinceChange.Seconds(), activityCooldown.Seconds())
			return "active", nil
		}
		// Not yet stabilized - still in initialization phase, show waiting
		s.lastStableStatus = "waiting"
		debugLog("%s: COOLDOWN ACTIVE but stabilized=false → waiting",
			shortName)
		return "waiting", nil
	}

	// Cooldown expired - content has been stable long enough
	// Mark session as stabilized (ready to show green on next activity)
	if !s.stateTracker.stabilized {
		s.stateTracker.stabilized = true
		debugLog("%s: STABILIZED (cooldown expired with stable content)", shortName)
	}

	// Return YELLOW or GRAY based on acknowledged state
	if s.stateTracker.acknowledged {
		s.lastStableStatus = "idle"
		debugLog("%s: COOLDOWN EXPIRED, ack=true → idle", shortName)
		return "idle", nil
	}

	s.lastStableStatus = "waiting"
	debugLog("%s: COOLDOWN EXPIRED, ack=false → waiting", shortName)
	return "waiting", nil
}

// Acknowledge marks the session as "seen" by the user
// Call this when user attaches to the session
func (s *Session) Acknowledge() {
	s.stateTrackerMu.Lock()
	defer s.stateTrackerMu.Unlock()

	s.ensureStateTrackerLocked()
	s.stateTracker.acknowledged = true
	s.lastStableStatus = "idle"
}

// ResetAcknowledged marks the session as needing attention
// Call this when a hook event indicates the agent finished (Stop, AfterAgent)
// This ensures the session shows yellow (waiting) instead of gray (idle)
func (s *Session) ResetAcknowledged() {
	s.stateTrackerMu.Lock()
	defer s.stateTrackerMu.Unlock()

	s.ensureStateTrackerLocked()
	s.stateTracker.acknowledged = false
	s.lastStableStatus = "waiting"
}

// analyzeContent checks content for updates, prompts, and busy indicators
// Returns: (contentUpdated, hasWaitingPrompt, hasBusyIndicator)
func (s *Session) analyzeContent() (updated bool, hasPrompt bool, isBusy bool) {
	content, err := s.CapturePane()
	if err != nil {
		return false, false, false
	}

	// Ensure we have a prompt detector for current tool
	tool := s.DetectTool()
	if s.promptDetector == nil || s.promptDetector.tool != tool {
		s.promptDetector = NewPromptDetector(tool)
	}

	// Check for explicit BUSY indicators (tool is actively processing)
	// This catches cases where Claude shows "esc to interrupt" but output hasn't changed
	isBusy = s.hasBusyIndicator(content)

	// Check for tool-specific waiting prompt
	hasPrompt = s.promptDetector.HasPrompt(content)

	// Check if content changed using hash comparison
	hash := s.hashContent(content)
	if s.lastHash == "" {
		s.lastHash = hash
		s.lastContent = content
		return true, hasPrompt, isBusy
	}

	if hash != s.lastHash {
		s.lastHash = hash
		s.lastContent = content
		return true, hasPrompt, isBusy
	}

	return false, hasPrompt, isBusy
}

// hasBusyIndicator checks if the terminal shows explicit busy indicators
// This is a quick check used in GetStatus() to detect active processing
//
// Busy indicators for different tools:
// - Claude Code: "esc to interrupt", spinner chars, "Thinking...", "Connecting..."
// - Gemini: Similar spinner patterns
// - Aider: Processing indicators
// - Shell: Running commands (no prompt visible)
func (s *Session) hasBusyIndicator(content string) bool {
	// Get last 10 lines for analysis
	lines := strings.Split(content, "\n")
	start := len(lines) - 10
	if start < 0 {
		start = 0
	}
	recentContent := strings.ToLower(strings.Join(lines[start:], "\n"))

	// ═══════════════════════════════════════════════════════════════════════
	// Text-based busy indicators
	// ═══════════════════════════════════════════════════════════════════════
	busyIndicators := []string{
		"esc to interrupt",   // Claude Code main indicator
		"(esc to interrupt)", // Claude Code in parentheses
		"· esc to interrupt", // With separator
	}

	for _, indicator := range busyIndicators {
		if strings.Contains(recentContent, indicator) {
			return true
		}
	}

	// Check for "Thinking... (Xs · Y tokens)" pattern
	if strings.Contains(recentContent, "thinking") && strings.Contains(recentContent, "tokens") {
		return true
	}

	// Check for "Connecting..." pattern
	if strings.Contains(recentContent, "connecting") && strings.Contains(recentContent, "tokens") {
		return true
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Spinner characters (from cli-spinners "dots" - used by Claude Code)
	// These braille characters animate to show processing
	// ═══════════════════════════════════════════════════════════════════════
	spinnerChars := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

	// Only check last 5 lines for spinners (they appear near the bottom)
	last5 := lines
	if len(last5) > 5 {
		last5 = last5[len(last5)-5:]
	}

	for _, line := range last5 {
		for _, spinner := range spinnerChars {
			if strings.Contains(line, spinner) {
				return true
			}
		}
	}

	// ═══════════════════════════════════════════════════════════════════════
	// Additional busy indicators (for other tools)
	// ═══════════════════════════════════════════════════════════════════════

	// Generic "working" indicators that appear in various tools
	workingIndicators := []string{
		"processing",
		"loading",
		"please wait",
		"working",
	}

	// Only match these if they're standalone (not part of other text)
	for _, indicator := range workingIndicators {
		// Check if indicator appears at start of a line (more reliable)
		for _, line := range last5 {
			lineLower := strings.ToLower(strings.TrimSpace(line))
			if strings.HasPrefix(lineLower, indicator) {
				return true
			}
		}
	}

	return false
}

// HasUpdatedWithPrompt checks if content changed AND if a prompt is present
// This is Claude Squad's exact HasUpdated() signature that returns (updated, hasPrompt)
func (s *Session) HasUpdatedWithPrompt() (updated bool, hasPrompt bool) {
	content, err := s.CapturePane()
	if err != nil {
		return false, false
	}

	// Ensure we have a prompt detector for current tool
	tool := s.DetectTool()
	if s.promptDetector == nil || s.promptDetector.tool != tool {
		s.promptDetector = NewPromptDetector(tool)
	}

	// Check for tool-specific prompt (Claude Squad's exact logic)
	hasPrompt = s.promptDetector.HasPrompt(content)

	// Check if content changed using hash comparison
	hash := s.hashContent(content)
	if s.lastHash == "" {
		s.lastHash = hash
		s.lastContent = content
		return true, hasPrompt
	}

	if hash != s.lastHash {
		s.lastHash = hash
		s.lastContent = content
		return true, hasPrompt
	}

	return false, hasPrompt
}

// Precompiled regex patterns for dynamic content stripping
// These are compiled once at package init for performance
var (
	// Matches Claude Code status line: "(45s · 1234 tokens · esc to interrupt)"
	dynamicStatusPattern = regexp.MustCompile(`\([^)]*\d+s\s*·[^)]*tokens[^)]*\)`)

	// Matches standalone time counters: "45s", "2m30s", "1h2m3s"
	timeCounterPattern = regexp.MustCompile(`\b\d+s\b`)

	// Matches token counts: "1234 tokens", "50 token"
	tokenCountPattern = regexp.MustCompile(`\b\d+\s*tokens?\b`)

	// Matches "Thinking..." or "Connecting..." with timing info
	thinkingPattern = regexp.MustCompile(`(Thinking|Connecting)[^(]*\([^)]*\)`)
)

// normalizeContent strips ANSI codes, spinner characters, and normalizes whitespace
// This is critical for stable hashing - prevents flickering from:
// 1. Color/style changes in terminal output
// 2. Animated spinner characters (⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏)
// 3. Other non-printing control characters
// 4. Terminal resize (which can add trailing spaces with tmux -J flag)
// 5. Multiple consecutive blank lines
// 6. Dynamic time/token counters (e.g., "45s · 1234 tokens")
func (s *Session) normalizeContent(content string) string {
	// Strip ANSI escape codes first (handles CSI, OSC, and C1 codes)
	result := StripANSI(content)

	// Strip other non-printing control characters
	result = stripControlChars(result)

	// Strip braille spinner characters (used by Claude Code and others)
	// These animate while processing and cause hash changes
	spinners := []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}
	for _, r := range spinners {
		result = strings.ReplaceAll(result, string(r), "")
	}

	// Strip dynamic time/token counters that change every second
	// This prevents flickering when Claude Code shows "(45s · 1234 tokens · esc to interrupt)"
	// which updates to "(46s · 1234 tokens · esc to interrupt)" one second later
	result = dynamicStatusPattern.ReplaceAllString(result, "(STATUS)")
	result = thinkingPattern.ReplaceAllString(result, "$1...")

	// Normalize trailing whitespace per line (fixes resize false positives)
	// tmux capture-pane -J can add trailing spaces when terminal is resized
	lines := strings.Split(result, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	result = strings.Join(lines, "\n")

	// Normalize multiple consecutive blank lines to a single blank line
	// This prevents hash changes from cursor position variations
	result = normalizeBlankLines(result)

	return result
}

// normalizeBlankLines collapses runs of 3+ newlines to 2 newlines (one blank line)
func normalizeBlankLines(content string) string {
	// Match 3 or more consecutive newlines and replace with 2
	re := regexp.MustCompile(`\n{3,}`)
	return re.ReplaceAllString(content, "\n\n")
}

// stripControlChars removes all ASCII control characters except for tab, newline,
// and carriage return. This helps stabilize content for hashing.
func stripControlChars(content string) string {
	var result strings.Builder
	result.Grow(len(content))
	for _, r := range content {
		// Keep printable characters (space and above), and essential whitespace.
		// DEL (127) is excluded.
		if (r >= 32 && r != 127) || r == '\t' || r == '\n' || r == '\r' {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// hashContent generates SHA256 hash of content (same as Claude Squad)
func (s *Session) hashContent(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// getWindowActivity returns the tmux window_activity timestamp (Unix time)
// This is a fast way to check if ANY output occurred - updates on every byte written
// Returns 0 if unable to get the timestamp
func (s *Session) getWindowActivity() int64 {
	cmd := exec.Command("tmux", "display-message", "-t", s.Name, "-p", "#{window_activity}")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}
	timestamp, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0
	}
	return timestamp
}

// SendKeys sends keys to the tmux session
func (s *Session) SendKeys(keys string) error {
	cmd := exec.Command("tmux", "send-keys", "-t", s.Name, keys)
	return cmd.Run()
}

// SendEnter sends an Enter key to the tmux session
func (s *Session) SendEnter() error {
	cmd := exec.Command("tmux", "send-keys", "-t", s.Name, "Enter")
	return cmd.Run()
}

// GetWorkDir returns the working directory of the session
func (s *Session) GetWorkDir() string {
	return s.WorkDir
}

// ListAllSessions returns all Agent Deck tmux sessions
func ListAllSessions() ([]*Session, error) {
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}")
	output, err := cmd.Output()
	if err != nil {
		// No sessions exist
		if strings.Contains(err.Error(), "no server running") ||
			strings.Contains(err.Error(), "no sessions") {
			return []*Session{}, nil
		}
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var sessions []*Session

	for _, line := range lines {
		if strings.HasPrefix(line, SessionPrefix) {
			displayName := strings.TrimPrefix(line, SessionPrefix)
			// Get session info
			sess := &Session{
				Name:        line,
				DisplayName: displayName,
			}
			// Try to get working directory
			workDirCmd := exec.Command("tmux", "display-message", "-t", line, "-p", "#{pane_current_path}")
			if workDirOutput, err := workDirCmd.Output(); err == nil {
				sess.WorkDir = strings.TrimSpace(string(workDirOutput))
			}
			sessions = append(sessions, sess)
		}
	}

	return sessions, nil
}

// DiscoverAllTmuxSessions returns all tmux sessions (including non-Agent Deck ones)
func DiscoverAllTmuxSessions() ([]*Session, error) {
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}:#{pane_current_path}")
	output, err := cmd.Output()
	if err != nil {
		// No sessions exist
		if strings.Contains(err.Error(), "no server running") ||
			strings.Contains(err.Error(), "no sessions") {
			return []*Session{}, nil
		}
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	var sessions []*Session

	for _, line := range lines {
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		sessionName := parts[0]
		workDir := ""
		if len(parts) == 2 {
			workDir = parts[1]
		}

		// Create session object
		sess := &Session{
			Name:        sessionName,
			DisplayName: sessionName,
			WorkDir:     workDir,
		}

		// If it's an agent-deck session, clean up the display name
		if strings.HasPrefix(sessionName, SessionPrefix) {
			sess.DisplayName = strings.TrimPrefix(sessionName, SessionPrefix)
		}

		sessions = append(sessions, sess)
	}

	return sessions, nil
}
