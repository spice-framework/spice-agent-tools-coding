// Package coding provides the bounded, instance-owned coding tool suite for
// Spice Agent.
package coding

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	defaultMaxReadBytes  int64 = 4 << 20
	defaultMaxWriteBytes int64 = 4 << 20
	defaultMaxOutput     int64 = 2 << 20
	maximumBytes         int64 = 64 << 20
	defaultTimeout             = 2 * time.Minute
)

// SecurityWarning describes the privilege boundary applications must display
// on first run and in help output.
const SecurityWarning = "Coding tools run with your process privileges; no sandbox or permission prompt is active."

// Config defines one bounded tool suite rooted at an absolute worktree path.
type Config struct {
	Root           string        `spice:"root,required,env=SPICE_AGENT_WORKTREE"`
	MaxReadBytes   int64         `spice:"max-read-bytes,default=4194304"`
	MaxWriteBytes  int64         `spice:"max-write-bytes,default=4194304"`
	MaxOutputBytes int64         `spice:"max-output-bytes,default=2097152"`
	CommandTimeout time.Duration `spice:"command-timeout,default=2m"`
}

// Risk identifies the privilege class disclosed by a capability.
type Risk string

const (
	// RiskRead allows bounded filesystem inspection.
	RiskRead Risk = "read"
	// RiskWrite allows bounded filesystem mutation.
	RiskWrite Risk = "write"
	// RiskProcess allows arbitrary child-process execution.
	RiskProcess Risk = "process"
)

// Capability describes one statically compiled coding operation. Capability
// metadata is informational and is not a permission boundary.
type Capability struct {
	Name        string
	Description string
	Risk        Risk
}

// Suite owns normalized bounds and deterministic capability metadata.
type Suite struct {
	config       Config
	capabilities []Capability
}

// New validates and constructs a tool suite without touching the filesystem or
// starting a process.
func New(config Config) (*Suite, error) {
	config = normalize(config)
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &Suite{
		config: config,
		capabilities: []Capability{
			{Name: "read", Description: "Read a bounded file within the configured worktree.", Risk: RiskRead},
			{Name: "replace", Description: "Atomically replace expected content within the configured worktree.", Risk: RiskWrite},
			{Name: "shell", Description: "Execute a bounded, cancelable process within the configured worktree.", Risk: RiskProcess},
		},
	}, nil
}

// Config returns the normalized immutable configuration value.
func (suite *Suite) Config() Config {
	return suite.config
}

// Capabilities returns a defensive copy ordered by stable tool name.
func (suite *Suite) Capabilities() []Capability {
	return slices.Clone(suite.capabilities)
}

func normalize(config Config) Config {
	config.Root = filepath.Clean(strings.TrimSpace(config.Root))
	if config.MaxReadBytes == 0 {
		config.MaxReadBytes = defaultMaxReadBytes
	}
	if config.MaxWriteBytes == 0 {
		config.MaxWriteBytes = defaultMaxWriteBytes
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = defaultMaxOutput
	}
	if config.CommandTimeout == 0 {
		config.CommandTimeout = defaultTimeout
	}
	return config
}

func (config Config) validate() error {
	if config.Root == "." || !filepath.IsAbs(config.Root) {
		return errors.New("coding tool root must be an absolute path")
	}
	limits := []struct {
		name  string
		value int64
	}{
		{name: "max read bytes", value: config.MaxReadBytes},
		{name: "max write bytes", value: config.MaxWriteBytes},
		{name: "max output bytes", value: config.MaxOutputBytes},
	}
	for _, limit := range limits {
		if limit.value <= 0 || limit.value > maximumBytes {
			return fmt.Errorf("coding tool %s must be between 1 and %d", limit.name, maximumBytes)
		}
	}
	if config.CommandTimeout <= 0 || config.CommandTimeout > 30*time.Minute {
		return errors.New("coding tool command timeout must be greater than zero and no more than 30m")
	}
	return nil
}
