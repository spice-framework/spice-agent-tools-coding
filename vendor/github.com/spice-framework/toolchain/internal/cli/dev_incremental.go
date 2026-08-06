package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	codegen "github.com/spice-framework/toolchain/compiler/generate"
	"github.com/spice-framework/toolchain/internal/devloop"
)

func (pipeline *developmentPipeline) reusableGeneration(
	batch devloop.Batch,
) (resultPlan codegen.Plan, resultTarget string, reused bool, resultErr error) {
	pipeline.cacheMu.Lock()
	if !pipeline.hasGenerationCache {
		pipeline.cacheMu.Unlock()
		return codegen.Plan{}, "", false, nil
	}
	plan := pipeline.cachedPlan
	targetName := pipeline.cachedTargetName
	structures := pipeline.cachedStructures
	pipeline.cacheMu.Unlock()

	root, err := os.OpenRoot(pipeline.root)
	if err != nil {
		return codegen.Plan{}, "", false, fmt.Errorf(
			"open development source root: %w",
			err,
		)
	}
	defer func() {
		resultErr = errors.Join(resultErr, root.Close())
	}()
	for _, change := range batch.Changes() {
		if change.Kind != devloop.ChangeWrite ||
			!strings.EqualFold(filepath.Ext(change.Path), ".go") {
			return codegen.Plan{}, "", false, nil
		}
		content, readErr := root.ReadFile(filepath.FromSlash(change.Path))
		if readErr != nil {
			return codegen.Plan{}, "", false, fmt.Errorf(
				"read changed development source %s: %w",
				change.Path,
				readErr,
			)
		}
		fingerprint, fingerprintErr := devloop.StructuralFingerprint(
			change.Path,
			content,
		)
		if fingerprintErr != nil {
			return codegen.Plan{}, "", false, fingerprintErr
		}
		previous, found := structures[filepath.ToSlash(change.Path)]
		if !found || previous != fingerprint {
			return codegen.Plan{}, "", false, nil
		}
	}
	return plan, targetName, true, nil
}

func (pipeline *developmentPipeline) rememberGeneration(
	ctx context.Context,
	plan codegen.Plan,
	targetName string,
) error {
	structures, err := structuralSnapshot(ctx, pipeline.root)
	if err != nil {
		return err
	}
	pipeline.cacheMu.Lock()
	defer pipeline.cacheMu.Unlock()
	pipeline.cachedPlan = plan
	pipeline.cachedTargetName = targetName
	pipeline.cachedStructures = structures
	pipeline.hasGenerationCache = true
	return nil
}

func structuralSnapshot(
	ctx context.Context,
	rootPath string,
) (result map[string][32]byte, resultErr error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open development source root: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, root.Close())
	}()
	result = make(map[string][32]byte)
	err = fs.WalkDir(root.FS(), ".", func(
		name string,
		entry fs.DirEntry,
		walkErr error,
	) error {
		if walkErr != nil {
			return walkErr
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		normalized := filepath.ToSlash(name)
		if entry.IsDir() {
			if name != "." && ignoredStructuralDirectory(normalized, entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(name), ".go") {
			return nil
		}
		content, readErr := root.ReadFile(name)
		if readErr != nil {
			return fmt.Errorf("read development source %s: %w", normalized, readErr)
		}
		fingerprint, fingerprintErr := devloop.StructuralFingerprint(normalized, content)
		if fingerprintErr != nil {
			return fingerprintErr
		}
		result[normalized] = fingerprint
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("snapshot development source structure: %w", err)
	}
	return result, nil
}

func ignoredStructuralDirectory(path, name string) bool {
	switch name {
	case ".git", ".spice", ".tmp", ".tools", "testdata", "vendor":
		return true
	default:
		return strings.Contains(path, "/internal/spicegen") ||
			strings.HasPrefix(path, "internal/spicegen")
	}
}
