package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/spice-framework/toolchain/compiler/load"
	compilerstarter "github.com/spice-framework/toolchain/compiler/starter"
	"github.com/spice-framework/toolchain/internal/moduleenv"
)

const (
	maxModuleGraphEntries = 10_000
	maxModuleGraphStderr  = 64 << 10
)

type goListModule struct {
	Path    string        `json:"Path"`
	Version string        `json:"Version"`
	Main    bool          `json:"Main"`
	Replace *goListModule `json:"Replace"`
}

func loadModuleVersions(
	ctx context.Context,
	options load.Options,
) ([]compilerstarter.ModuleVersion, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, found := moduleenv.VendorRoot(options.Dir, options.Env); found {
		workspace, err := moduleenv.WorkspaceModules(ctx, options.Dir, options.Env)
		if err != nil {
			return nil, err
		}
		vendored, err := moduleenv.VendoredModules(ctx, options.Dir, options.Env)
		if err != nil {
			return nil, err
		}
		return moduleVersionsFromVendor(workspace, vendored)
	}

	// #nosec G204 -- the executable and every argument are compiler-owned constants.
	command := exec.CommandContext(
		ctx,
		"go",
		"list",
		"-mod="+moduleenv.OfflineMode(options.Dir, options.Env),
		"-m",
		"-json",
		"all",
	)
	command.Dir = options.Dir
	command.Env = moduleGraphEnvironment(options.Env)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open go list output: %w", err)
	}
	stderr := newBoundedBuffer(maxModuleGraphStderr)
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start offline go module inspection: %w", err)
	}

	modules, decodeErr := decodeModuleVersions(stdout)
	if decodeErr != nil && command.Process != nil {
		if killErr := command.Process.Kill(); killErr != nil &&
			!errors.Is(killErr, os.ErrProcessDone) {
			decodeErr = errors.Join(
				decodeErr,
				fmt.Errorf("stop invalid module graph inspection: %w", killErr),
			)
		}
	}
	waitErr := command.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("decode offline go module graph: %w", decodeErr)
	}
	if waitErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return nil, fmt.Errorf("inspect offline Go module graph: %w", waitErr)
		}
		return nil, fmt.Errorf(
			"inspect offline Go module graph: %w: %s",
			waitErr,
			detail,
		)
	}
	return modules, nil
}

func moduleVersionsFromVendor(
	workspace []moduleenv.WorkspaceModule,
	vendored []moduleenv.VendoredModule,
) ([]compilerstarter.ModuleVersion, error) {
	modules := make([]compilerstarter.ModuleVersion, 0, len(workspace)+len(vendored))
	seen := make(map[string]struct{}, cap(modules))
	appendModule := func(value compilerstarter.ModuleVersion) error {
		if _, duplicate := seen[value.Path]; duplicate {
			return fmt.Errorf("vendored Go module graph contains duplicate module path %q", value.Path)
		}
		if len(modules) == maxModuleGraphEntries {
			return fmt.Errorf(
				"vendored module graph exceeds the %d-entry safety limit",
				maxModuleGraphEntries,
			)
		}
		seen[value.Path] = struct{}{}
		modules = append(modules, value)
		return nil
	}
	for _, item := range workspace {
		if err := appendModule(compilerstarter.ModuleVersion{Path: item.Path, Main: true}); err != nil {
			return nil, err
		}
	}
	for _, item := range vendored {
		if err := appendModule(compilerstarter.ModuleVersion{
			Path:               item.Path,
			Version:            item.Version,
			ReplacementPath:    item.ReplacementPath,
			ReplacementVersion: item.ReplacementVersion,
		}); err != nil {
			return nil, err
		}
	}
	sort.Slice(modules, func(left, right int) bool {
		if modules[left].Main != modules[right].Main {
			return modules[left].Main
		}
		return modules[left].Path < modules[right].Path
	})
	return modules, nil
}

func decodeModuleVersions(
	reader io.Reader,
) ([]compilerstarter.ModuleVersion, error) {
	decoder := json.NewDecoder(reader)
	modules := make([]compilerstarter.ModuleVersion, 0)
	for {
		var listed goListModule
		err := decoder.Decode(&listed)
		if errors.Is(err, io.EOF) {
			return modules, nil
		}
		if err != nil {
			return nil, err
		}
		if len(modules) == maxModuleGraphEntries {
			return nil, fmt.Errorf(
				"module graph exceeds the %d-entry safety limit",
				maxModuleGraphEntries,
			)
		}
		module := compilerstarter.ModuleVersion{
			Path:    listed.Path,
			Version: listed.Version,
			Main:    listed.Main,
		}
		if listed.Replace != nil {
			module.ReplacementPath = listed.Replace.Path
			module.ReplacementVersion = listed.Replace.Version
		}
		modules = append(modules, module)
	}
}

func moduleGraphEnvironment(environment []string) []string {
	if environment == nil {
		environment = os.Environ()
	}
	result := make([]string, 0, len(environment)+2)
	for _, value := range environment {
		name, _, found := strings.Cut(value, "=")
		if found &&
			(strings.EqualFold(name, "GOPROXY") ||
				strings.EqualFold(name, "GOSUMDB")) {
			continue
		}
		result = append(result, value)
	}
	return append(result, "GOPROXY=off", "GOSUMDB=off")
}

type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := max(buffer.limit-buffer.buffer.Len(), 0)
	if len(value) > remaining {
		value = value[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.buffer.Write(value)
	return originalLength, nil
}

func (buffer *boundedBuffer) String() string {
	result := buffer.buffer.String()
	if buffer.truncated {
		result += "\n... output truncated"
	}
	return result
}
