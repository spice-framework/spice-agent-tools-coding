// Package coding provides the bounded, instance-owned coding tool suite for
// Spice Agent.
package coding

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spice-framework/spice-agent/tool"
)

const (
	defaultMaxReadBytes  int64 = 256 << 10
	defaultMaxWriteBytes int64 = 256 << 10
	defaultMaxOutput     int64 = 256 << 10
	maximumBytes         int64 = tool.MaximumPayloadBytes / 2
	defaultTimeout             = 2 * time.Minute
)

// SecurityWarning describes the privilege boundary applications must display
// on first run and in help output.
const SecurityWarning = "Coding tools can read and write files, execute processes, and use network or environment access with your operating-system privileges; no sandbox or approval prompt is active."

// Config defines one bounded tool suite rooted at an absolute worktree path.
type Config struct {
	Root           string        `spice:"root,required,env=SPICE_AGENT_WORKTREE"`
	MaxReadBytes   int64         `spice:"max-read-bytes,default=262144"`
	MaxWriteBytes  int64         `spice:"max-write-bytes,default=262144"`
	MaxOutputBytes int64         `spice:"max-output-bytes,default=262144"`
	CommandTimeout time.Duration `spice:"command-timeout,default=2m"`
	// EnvironmentAllowlist names host variables inherited by shell children.
	// Values never appear in tool arguments or results.
	EnvironmentAllowlist []string `spice:"environment-allowlist"`
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
			{Name: "shell", Description: "Execute an unsandboxed bounded process with the user's privileges.", Risk: RiskProcess},
		},
	}, nil
}

// Config returns the normalized immutable configuration value.
func (suite *Suite) Config() Config {
	return cloneConfig(suite.config)
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
	if len(config.EnvironmentAllowlist) == 0 {
		config.EnvironmentAllowlist = defaultEnvironmentAllowlist()
	}
	config.EnvironmentAllowlist = normalizeEnvironmentAllowlist(config.EnvironmentAllowlist)
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
	for _, name := range config.EnvironmentAllowlist {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return fmt.Errorf("coding tool environment name %q is invalid", name)
		}
	}
	return nil
}

func cloneConfig(config Config) Config {
	config.EnvironmentAllowlist = slices.Clone(config.EnvironmentAllowlist)
	return config
}

func normalizeEnvironmentAllowlist(names []string) []string {
	result := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, value := range names {
		name := strings.TrimSpace(value)
		identity := name
		if runtime.GOOS == "windows" {
			identity = strings.ToUpper(name)
			name = identity
		}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}

func defaultEnvironmentAllowlist() []string {
	if runtime.GOOS == "windows" {
		return []string{"COMSPEC", "PATH", "PATHEXT", "SYSTEMROOT", "TEMP", "TMP", "WINDIR"}
	}
	return []string{"HOME", "LANG", "PATH", "TMPDIR"}
}
