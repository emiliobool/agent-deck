package session

import (
	"os/exec"
	"testing"
	"time"
)

// TestNewSessionStatusFlicker tests for green flicker on new session creation
// This reproduces the issue where a session briefly shows green before first poll
func TestNewSessionStatusFlicker(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	// Create a new session with a command (like user would do)
	inst := NewInstance("test-flicker", "/tmp")
	inst.Command = "echo hello" // Non-empty command

	// BEFORE Start() - should be idle
	if inst.Status != StatusIdle {
		t.Errorf("Before Start(): Status = %s, want idle", inst.Status)
	}

	// After Start() - current behavior sets StatusRunning immediately
	// This is the source of the flicker!
	err := inst.Start()
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() { _ = inst.Kill() }()

	t.Logf("After Start(): Status = %s", inst.Status)

	// Current behavior: StatusRunning is set in Start() if Command != ""
	// This causes a brief GREEN flash before the first GetStatus() poll
	if inst.Status == StatusRunning {
		t.Log("WARNING: FLICKER SOURCE - Status is 'running' immediately after Start()")
		t.Log("         This shows GREEN before the first tick updates it to the actual status")
	}

	// Simulate first tick (what happens 0-500ms after creation)
	err = inst.UpdateStatus()
	if err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	t.Logf("After first UpdateStatus(): Status = %s", inst.Status)

	// After first poll, status should be 'waiting' (not 'running')
	// because GetStatus() returns "waiting" on first poll
	if inst.Status == StatusWaiting {
		t.Log("OK: First poll correctly shows 'waiting' (yellow)")
	}
}

// TestInstance_CanFork tests the CanFork method for Claude session forking
func TestInstance_CanFork(t *testing.T) {
	inst := NewInstance("test", "/tmp/test")

	// Without Claude session ID, cannot fork
	if inst.CanFork() {
		t.Error("CanFork() should be false without ClaudeSessionID")
	}

	// With Claude session ID, can fork
	inst.ClaudeSessionID = "abc-123-def"
	inst.ClaudeDetectedAt = time.Now()
	if !inst.CanFork() {
		t.Error("CanFork() should be true with recent ClaudeSessionID")
	}

	// With old detection time, cannot fork (stale)
	inst.ClaudeDetectedAt = time.Now().Add(-10 * time.Minute)
	if inst.CanFork() {
		t.Error("CanFork() should be false with stale ClaudeSessionID")
	}
}

// TestInstance_UpdateClaudeSession tests the UpdateClaudeSession method
func TestInstance_UpdateClaudeSession(t *testing.T) {
	inst := NewInstance("test", "/tmp/test")
	inst.Tool = "claude"

	// Mock: In real test, would need actual Claude running
	// For now, just test the method exists and doesn't crash
	inst.UpdateClaudeSession()

	// After update with no Claude running, should have no session ID
	// (In integration test, would verify actual detection)
}

// TestInstance_Fork tests the Fork method
func TestInstance_Fork(t *testing.T) {
	inst := NewInstance("test", "/tmp/test")

	// Cannot fork without session ID
	_, err := inst.Fork("forked-test", "")
	if err == nil {
		t.Error("Fork() should fail without ClaudeSessionID")
	}

	// With session ID, Fork returns command to run
	inst.ClaudeSessionID = "abc-123"
	inst.ClaudeDetectedAt = time.Now()
	cmd, err := inst.Fork("forked-test", "")
	if err != nil {
		t.Errorf("Fork() failed: %v", err)
	}

	expected := "claude --resume abc-123 --fork-session"
	if cmd != expected {
		t.Errorf("Fork() = %s, want %s", cmd, expected)
	}
}

// TestInstance_CreateForkedInstance tests the CreateForkedInstance method
func TestInstance_CreateForkedInstance(t *testing.T) {
	inst := NewInstance("original", "/tmp/test")
	inst.GroupPath = "projects"

	// Cannot create fork without session ID
	_, _, err := inst.CreateForkedInstance("forked", "")
	if err == nil {
		t.Error("CreateForkedInstance() should fail without ClaudeSessionID")
	}

	// With session ID, creates new instance with fork command
	inst.ClaudeSessionID = "abc-123"
	inst.ClaudeDetectedAt = time.Now()
	forked, cmd, err := inst.CreateForkedInstance("forked", "")
	if err != nil {
		t.Errorf("CreateForkedInstance() failed: %v", err)
	}

	// Verify command is correct
	expectedCmd := "claude --resume abc-123 --fork-session"
	if cmd != expectedCmd {
		t.Errorf("Command = %s, want %s", cmd, expectedCmd)
	}

	// Verify forked instance has correct properties
	if forked.Title != "forked" {
		t.Errorf("Forked title = %s, want forked", forked.Title)
	}
	if forked.ProjectPath != "/tmp/test" {
		t.Errorf("Forked path = %s, want /tmp/test", forked.ProjectPath)
	}
	if forked.GroupPath != "projects" {
		t.Errorf("Forked group = %s, want projects (inherited)", forked.GroupPath)
	}
	if forked.Command != expectedCmd {
		t.Errorf("Forked command = %s, want %s", forked.Command, expectedCmd)
	}
	if forked.Tool != "claude" {
		t.Errorf("Forked tool = %s, want claude", forked.Tool)
	}

	// Test with custom group path
	forked2, _, err := inst.CreateForkedInstance("forked2", "custom-group")
	if err != nil {
		t.Errorf("CreateForkedInstance() with custom group failed: %v", err)
	}
	if forked2.GroupPath != "custom-group" {
		t.Errorf("Forked group = %s, want custom-group", forked2.GroupPath)
	}
}
