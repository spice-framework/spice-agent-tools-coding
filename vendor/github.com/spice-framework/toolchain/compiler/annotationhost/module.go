// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package annotationhost authorizes and hosts Go-native annotation tools.
//
// @NamedInterface("annotationhost")
package annotationhost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/mod/modfile"
)

// TargetModule is the exact application-owned authorization boundary.
type TargetModule struct {
	Root      string
	Path      string
	GoVersion string
	Tools     []string
}

// ReadTargetModule parses the go.mod physically owned by root. It does not
// search parent directories or accept authorization from go.work.
func ReadTargetModule(root string) (TargetModule, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return TargetModule{}, fmt.Errorf(
			"resolve target application module root: %w",
			err,
		)
	}
	directory, err := os.OpenRoot(absolute)
	if err != nil {
		return TargetModule{}, fmt.Errorf(
			"open target application module root: %w",
			err,
		)
	}
	content, readErr := directory.ReadFile("go.mod")
	closeErr := directory.Close()
	if joinedErr := errors.Join(readErr, closeErr); joinedErr != nil {
		return TargetModule{}, fmt.Errorf(
			"read target application go.mod: %w",
			joinedErr,
		)
	}
	file, err := modfile.Parse("go.mod", content, nil)
	if err != nil {
		return TargetModule{}, fmt.Errorf(
			"parse target application go.mod: %w",
			err,
		)
	}
	if file.Module == nil || file.Module.Mod.Path == "" {
		return TargetModule{}, errors.New(
			"target application go.mod requires a module directive",
		)
	}
	tools := make([]string, 0, len(file.Tool))
	for _, tool := range file.Tool {
		if tool != nil && tool.Path != "" {
			tools = append(tools, tool.Path)
		}
	}
	sort.Strings(tools)
	tools = compactStrings(tools)
	goVersion := ""
	if file.Go != nil {
		goVersion = file.Go.Version
	}
	return TargetModule{
		Root:      absolute,
		Path:      file.Module.Mod.Path,
		GoVersion: goVersion,
		Tools:     tools,
	}, nil
}

// AuthorizeTool requires an exact tool declaration in the target module.
func (module TargetModule) AuthorizeTool(toolPath string) error {
	index := sort.SearchStrings(module.Tools, toolPath)
	if index < len(module.Tools) && module.Tools[index] == toolPath {
		return nil
	}
	return fmt.Errorf(
		"annotation tool %q is not authorized by %s; add it with 'go get -tool %s@<version>'",
		toolPath,
		filepath.Join(module.Root, "go.mod"),
		toolPath,
	)
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
