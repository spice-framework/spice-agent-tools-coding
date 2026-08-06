package coding

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type relativePath struct {
	display string
	native  string
}

func openWorktree(root string) (*os.Root, error) {
	workspace, err := os.OpenRoot(root)
	if err != nil {
		return nil, executionFailure("root_unavailable", "configured worktree root is unavailable")
	}
	info, err := workspace.Stat(".")
	if err != nil || !info.IsDir() {
		closeBestEffort(workspace)
		return nil, executionFailure("root_unavailable", "configured worktree root is unavailable")
	}
	return workspace, nil
}

func parseRelativePath(requested string, allowRoot bool) (relativePath, error) {
	if requested == "" || requested != strings.TrimSpace(requested) || strings.IndexByte(requested, 0) >= 0 {
		return relativePath{}, invalidArguments("path must be a non-empty relative path without surrounding whitespace")
	}
	native := filepath.Clean(filepath.FromSlash(requested))
	if filepath.IsAbs(native) || filepath.VolumeName(native) != "" || native == ".." ||
		strings.HasPrefix(native, ".."+string(filepath.Separator)) {
		return relativePath{}, invalidArguments("path must be relative to the configured worktree")
	}
	if native == "." && !allowRoot {
		return relativePath{}, invalidArguments("path must identify a file within the configured worktree")
	}
	return relativePath{display: filepath.ToSlash(native), native: native}, nil
}

func openRegularFile(workspace *os.Root, path relativePath) (*os.File, os.FileInfo, error) {
	file, err := workspace.Open(path.native)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, executionFailure("not_found", "requested path does not exist")
		}
		return nil, nil, executionFailure("path_unavailable", "requested path is unavailable or escapes the configured worktree")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		closeBestEffort(file)
		return nil, nil, executionFailure("not_regular", "requested path is not a regular file")
	}
	return file, info, nil
}

func rejectSymlinkComponents(workspace *os.Root, path relativePath) error {
	if path.native == "." {
		return nil
	}
	current := ""
	for component := range strings.SplitSeq(path.native, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := workspace.Lstat(current)
		if err != nil {
			return executionFailure("path_unavailable", "requested path is unavailable or escapes the configured worktree")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return executionFailure("path_escape", "symbolic-link workdir components are not allowed")
		}
	}
	return nil
}
