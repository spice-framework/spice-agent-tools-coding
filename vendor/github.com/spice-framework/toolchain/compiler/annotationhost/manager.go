package annotationhost

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Manager owns persistent per-workspace tool processes.
type Manager struct {
	mu      sync.Mutex
	clients map[string]*Client
}

// NewManager creates an empty process manager.
func NewManager() *Manager {
	return &Manager{clients: make(map[string]*Client)}
}

// Client returns an existing healthy workspace/tool process or starts it once.
func (manager *Manager) Client(
	ctx context.Context,
	config Config,
) (*Client, error) {
	if manager == nil {
		return nil, errors.New("annotation tool manager is nil")
	}
	key := managerKey(config.Root, config.ToolPath)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if client, found := manager.clients[key]; found && client.healthy() {
		return client, nil
	}
	client, err := Start(ctx, config)
	if err != nil {
		delete(manager.clients, key)
		return nil, err
	}
	manager.clients[key] = client
	return client, nil
}

// CloseWorkspace gracefully closes every process owned by root.
func (manager *Manager) CloseWorkspace(
	ctx context.Context,
	root string,
) error {
	if manager == nil {
		return nil
	}
	prefix := managerKey(root, "")
	manager.mu.Lock()
	var keys []string
	for key := range manager.clients {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	clients := make([]*Client, 0, len(keys))
	for _, key := range keys {
		clients = append(clients, manager.clients[key])
		delete(manager.clients, key)
	}
	manager.mu.Unlock()
	var result error
	for _, client := range clients {
		result = errors.Join(result, client.Close(ctx))
	}
	return result
}

// Close gracefully closes all owned processes.
func (manager *Manager) Close(ctx context.Context) error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	keys := make([]string, 0, len(manager.clients))
	for key := range manager.clients {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	clients := make([]*Client, 0, len(keys))
	for _, key := range keys {
		clients = append(clients, manager.clients[key])
		delete(manager.clients, key)
	}
	manager.mu.Unlock()
	var result error
	for _, client := range clients {
		result = errors.Join(result, client.Close(ctx))
	}
	return result
}

func managerKey(root, tool string) string {
	root = filepath.Clean(strings.TrimSpace(root))
	if runtime.GOOS == "windows" {
		root = strings.ToLower(root)
	}
	return root + "\x00" + tool
}

func (client *Client) healthy() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return !client.closed
}
