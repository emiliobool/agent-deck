package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// StorageData represents the JSON structure for persistence
type StorageData struct {
	Instances []*InstanceData `json:"instances"`
	Groups    []*GroupData    `json:"groups,omitempty"` // Persist empty groups
	UpdatedAt time.Time       `json:"updated_at"`
}

// InstanceData represents the serializable session data
type InstanceData struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	ProjectPath string    `json:"project_path"`
	GroupPath   string    `json:"group_path"`
	Command     string    `json:"command"`
	Tool        string    `json:"tool"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	TmuxSession string    `json:"tmux_session"`
}

// GroupData represents serializable group data
type GroupData struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Expanded bool   `json:"expanded"`
	Order    int    `json:"order"`
}

// Storage handles persistence of session data
type Storage struct {
	path string
}

// NewStorage creates a new storage instance
func NewStorage() (*Storage, error) {
	path, err := GetStoragePath()
	if err != nil {
		return nil, err
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}

	return &Storage{
		path: path,
	}, nil
}

// Save persists instances to JSON file
func (s *Storage) Save(instances []*Instance) error {
	return s.SaveWithGroups(instances, nil)
}

// SaveWithGroups persists instances and groups to JSON file
func (s *Storage) SaveWithGroups(instances []*Instance, groupTree *GroupTree) error {
	// Convert instances to serializable format
	data := StorageData{
		Instances: make([]*InstanceData, len(instances)),
		UpdatedAt: time.Now(),
	}

	for i, inst := range instances {
		tmuxName := ""
		if inst.tmuxSession != nil {
			tmuxName = inst.tmuxSession.Name
		}
		data.Instances[i] = &InstanceData{
			ID:          inst.ID,
			Title:       inst.Title,
			ProjectPath: inst.ProjectPath,
			GroupPath:   inst.GroupPath,
			Command:     inst.Command,
			Tool:        inst.Tool,
			Status:      inst.Status,
			CreatedAt:   inst.CreatedAt,
			TmuxSession: tmuxName,
		}
	}

	// Save groups (including empty ones)
	if groupTree != nil {
		data.Groups = make([]*GroupData, 0, len(groupTree.GroupList))
		for _, g := range groupTree.GroupList {
			data.Groups = append(data.Groups, &GroupData{
				Name:     g.Name,
				Path:     g.Path,
				Expanded: g.Expanded,
				Order:    g.Order,
			})
		}
	}

	// Marshal to JSON
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	// Write to file
	if err := os.WriteFile(s.path, jsonData, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// Load reads instances from JSON file
func (s *Storage) Load() ([]*Instance, error) {
	instances, _, err := s.LoadWithGroups()
	return instances, err
}

// LoadWithGroups reads instances and groups from JSON file
func (s *Storage) LoadWithGroups() ([]*Instance, []*GroupData, error) {
	// Check if file exists
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return []*Instance{}, nil, nil
	}

	// Read file
	jsonData, err := os.ReadFile(s.path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read file: %w", err)
	}

	// Unmarshal JSON
	var data StorageData
	if err := json.Unmarshal(jsonData, &data); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	// Convert to instances
	instances := make([]*Instance, len(data.Instances))
	for i, instData := range data.Instances {
		// Recreate tmux session object from stored name
		// Use ReconnectSessionWithStatus to restore the exact status state
		var tmuxSess *tmux.Session
		if instData.TmuxSession != "" {
			// Convert Status enum to string for tmux package
			// This restores the exact status across app restarts
			previousStatus := statusToString(instData.Status)
			tmuxSess = tmux.ReconnectSessionWithStatus(
				instData.TmuxSession,
				instData.Title,
				instData.ProjectPath,
				instData.Command,
				previousStatus,
			)
			// Enable mouse mode for proper scrolling (idempotent - safe to call multiple times)
			tmuxSess.EnableMouseMode()
		}

		// Migrate old sessions without GroupPath
		groupPath := instData.GroupPath
		if groupPath == "" {
			groupPath = extractGroupPath(instData.ProjectPath)
		}

		instances[i] = &Instance{
			ID:          instData.ID,
			Title:       instData.Title,
			ProjectPath: instData.ProjectPath,
			GroupPath:   groupPath,
			Command:     instData.Command,
			Tool:        instData.Tool,
			Status:      instData.Status,
			CreatedAt:   instData.CreatedAt,
			tmuxSession: tmuxSess,
		}
	}

	return instances, data.Groups, nil
}

// GetStoragePath returns the path to the sessions.json file
func GetStoragePath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}

	return filepath.Join(homeDir, ".agent-deck", "sessions.json"), nil
}

// statusToString converts a Status enum to the string expected by tmux.ReconnectSessionWithStatus
func statusToString(s Status) string {
	switch s {
	case StatusRunning:
		return "active"
	case StatusWaiting:
		return "waiting"
	case StatusIdle:
		return "idle"
	case StatusError:
		return "waiting" // Treat errors as needing attention
	default:
		return "waiting"
	}
}
