// @import { NamedInterface } from "github.com/spice-framework/spice/annotation/modulith"

// Package annotationinstall plans and applies hash-guarded Go module changes.
// It supports both explicit annotation-tool authorization and ordinary module
// dependencies through temporary modfiles and standard Go commands.
//
// @NamedInterface("annotationinstall")
package annotationinstall

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	maximumModuleFileBytes = 16 << 20
	maximumCommandOutput   = 256 << 10
	maximumPreviewDiff     = 1 << 20
	temporaryPrefix        = ".spice-tool-preview-"
)

// ErrStalePreview reports that a module file changed after the preview.
var ErrStalePreview = errors.New(
	"module change preview is stale",
)

// Preview is an immutable guarded standard Go module change. Its exact
// after-images are private so callers can apply only a plan produced here.
type Preview struct {
	root    string
	path    string
	version string
	kind    previewKind
	command string
	diff    string
	token   string
	guards  []fileGuard
	changes []fileChange
}

type previewKind string

const (
	previewTool       previewKind = "tool"
	previewDependency previewKind = "dependency"
)

type fileGuard struct {
	name  string
	state fileState
}

type fileChange struct {
	name   string
	before fileState
	after  fileState
	mode   fs.FileMode
}

type fileState struct {
	exists  bool
	content []byte
	hash    [sha256.Size]byte
}

// Root returns the normalized target application module root.
func (preview Preview) Root() string {
	return preview.root
}

// Tool returns the fully qualified Go tool package path.
func (preview Preview) Tool() string {
	if preview.kind != previewTool {
		return ""
	}
	return preview.path
}

// Dependency returns the ordinary package or module path selected by this
// preview, or an empty string for annotation-tool previews.
func (preview Preview) Dependency() string {
	if preview.kind != previewDependency {
		return ""
	}
	return preview.path
}

// Version returns the selected module version, when one was required.
func (preview Preview) Version() string {
	return preview.version
}

// Command returns the developer-facing standard Go command represented by the
// preview. Internal temporary-modfile flags are intentionally omitted.
func (preview Preview) Command() string {
	return preview.command
}

// Diff returns the exact deterministic go.mod/go.sum unified preview.
func (preview Preview) Diff() string {
	return preview.diff
}

// Token returns the content-derived identity used by confirmed editor actions.
func (preview Preview) Token() string {
	return preview.token
}

// PreviewTool runs go get -tool against a temporary sibling modfile. It never
// changes the application's go.mod or go.sum.
func PreviewTool(
	ctx context.Context,
	root string,
	toolPath string,
	version string,
	environment []string,
) (Preview, error) {
	return previewModuleChange(
		ctx,
		root,
		toolPath,
		version,
		environment,
		previewTool,
	)
}

// PreviewDependency runs go get against a temporary sibling modfile. It never
// changes the application's go.mod or go.sum.
func PreviewDependency(
	ctx context.Context,
	root string,
	dependencyPath string,
	version string,
	environment []string,
) (Preview, error) {
	return previewModuleChange(
		ctx,
		root,
		dependencyPath,
		version,
		environment,
		previewDependency,
	)
}

func previewModuleChange(
	ctx context.Context,
	root string,
	selectedPath string,
	version string,
	environment []string,
	kind previewKind,
) (Preview, error) {
	if ctx == nil {
		return Preview{}, errors.New(
			"module change preview context must not be nil",
		)
	}
	if kind != previewTool && kind != previewDependency {
		return Preview{}, errors.New("module change preview kind is invalid")
	}
	if err := module.CheckImportPath(selectedPath); err != nil {
		return Preview{}, fmt.Errorf(
			"validate %s package %q: %w",
			previewLabel(kind),
			selectedPath,
			err,
		)
	}
	if version != "" && !semver.IsValid(version) {
		return Preview{}, fmt.Errorf(
			"%s version %q is not a valid semantic version",
			previewLabel(kind),
			version,
		)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return Preview{}, fmt.Errorf(
			"resolve module change preview root: %w",
			err,
		)
	}
	directory, err := os.OpenRoot(absolute)
	if err != nil {
		return Preview{}, fmt.Errorf(
			"open module change preview root: %w",
			err,
		)
	}
	preview, previewErr := previewInRoot(
		ctx,
		absolute,
		directory,
		selectedPath,
		version,
		environment,
		kind,
	)
	closeErr := directory.Close()
	return preview, errors.Join(previewErr, closeErr)
}

func previewInRoot(
	ctx context.Context,
	absolute string,
	directory *os.Root,
	selectedPath string,
	version string,
	environment []string,
	kind previewKind,
) (Preview, error) {
	before, err := readModuleStates(directory)
	if err != nil {
		return Preview{}, err
	}
	temporary, err := createTemporaryModfile(
		absolute,
		directory,
		before,
	)
	if err != nil {
		return Preview{}, err
	}
	preview, previewErr := runPreviewCommand(
		ctx,
		absolute,
		directory,
		temporary,
		before,
		selectedPath,
		version,
		environment,
		kind,
	)
	cleanupErr := temporary.cleanup(directory)
	return preview, errors.Join(previewErr, cleanupErr)
}

func runPreviewCommand(
	ctx context.Context,
	absolute string,
	directory *os.Root,
	temporary temporaryModfile,
	before map[string]fileChange,
	selectedPath string,
	version string,
	environment []string,
	kind previewKind,
) (Preview, error) {
	selector := selectedPath
	if version != "" {
		selector += "@" + version
	}
	arguments := []string{"get"}
	if kind == previewTool {
		arguments = append(arguments, "-tool")
	}
	arguments = append(arguments, "-modfile="+temporary.modName, selector)
	command := exec.CommandContext( // #nosec G204 -- the executable and flags are fixed; the validated package/version is one argument.
		ctx,
		"go",
		arguments...,
	)
	command.Dir = absolute
	command.Env = installEnvironment(environment)
	stdout := newBoundedBuffer(maximumCommandOutput)
	stderr := newBoundedBuffer(maximumCommandOutput)
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	if contextErr := ctx.Err(); contextErr != nil {
		return Preview{}, contextErr
	}
	if runErr != nil {
		return Preview{}, fmt.Errorf(
			"preview %s: %w%s",
			developerCommand(kind, selector),
			runErr,
			commandFailureDetail(stdout, stderr),
		)
	}
	if stdout.overflow || stderr.overflow {
		return Preview{}, errors.New(
			"module change preview command output exceeded 256 KiB",
		)
	}
	after, err := temporary.readStates(directory)
	if err != nil {
		return Preview{}, err
	}
	if kind == previewTool && !moduleAuthorizesTool(
		after["go.mod"].after.content,
		selectedPath,
	) {
		return Preview{}, fmt.Errorf(
			"%s did not add the exact tool directive",
			developerCommand(kind, selector),
		)
	}
	changes := changedModuleFiles(before, after)
	if len(changes) == 0 {
		return Preview{}, fmt.Errorf(
			"%s produced no module-file changes",
			developerCommand(kind, selector),
		)
	}
	preview := Preview{
		root:    filepath.Clean(absolute),
		path:    selectedPath,
		version: version,
		kind:    kind,
		command: developerCommand(kind, selector),
		guards:  moduleFileGuards(before),
		changes: changes,
	}
	preview.diff = moduleDiff(changes)
	if len(preview.diff) > maximumPreviewDiff {
		return Preview{}, fmt.Errorf(
			"annotation tool module-file diff is %d bytes; maximum preview is %d bytes",
			len(preview.diff),
			maximumPreviewDiff,
		)
	}
	preview.token = previewToken(preview)
	return preview, nil
}

// Apply writes the exact previewed module-file after-images only if every
// original file still has the hash observed by PreviewTool.
func Apply(ctx context.Context, preview Preview) error {
	if ctx == nil {
		return errors.New(
			"annotation tool apply context must not be nil",
		)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if preview.root == "" ||
		preview.path == "" ||
		preview.kind != previewTool && preview.kind != previewDependency ||
		len(preview.guards) != 2 ||
		len(preview.changes) == 0 ||
		preview.token == "" ||
		preview.token != previewToken(preview) {
		return errors.New("module change preview is invalid")
	}
	root, err := os.OpenRoot(preview.root)
	if err != nil {
		return fmt.Errorf(
			"open annotation tool apply root: %w",
			err,
		)
	}
	applyErr := applyPreviewInRoot(ctx, root, preview)
	closeErr := root.Close()
	return errors.Join(applyErr, closeErr)
}

func applyPreviewInRoot(
	ctx context.Context,
	root *os.Root,
	preview Preview,
) error {
	for _, guard := range preview.guards {
		current, _, readErr := readModuleFile(root, guard.name, false)
		if readErr != nil {
			return readErr
		}
		if !sameFileState(current, guard.state) {
			return fmt.Errorf(
				"%w: %s changed after preview",
				ErrStalePreview,
				guard.name,
			)
		}
	}
	return applyChanges(ctx, root, preview.changes)
}

type temporaryModfile struct {
	modName string
	sumName string
}

func createTemporaryModfile(
	rootPath string,
	root *os.Root,
	before map[string]fileChange,
) (temporaryModfile, error) {
	file, err := os.CreateTemp(
		rootPath,
		temporaryPrefix+"*.mod",
	)
	if err != nil {
		return temporaryModfile{}, fmt.Errorf(
			"create annotation tool preview modfile: %w",
			err,
		)
	}
	name := filepath.Base(file.Name())
	writeErr := writeAll(file, before["go.mod"].before.content)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		removeErr := removeIfExists(root, name)
		return temporaryModfile{}, fmt.Errorf(
			"write annotation tool preview modfile: %w",
			errors.Join(err, removeErr),
		)
	}
	temporary := temporaryModfile{
		modName: name,
		sumName: strings.TrimSuffix(name, ".mod") + ".sum",
	}
	goSum := before["go.sum"].before
	if goSum.exists {
		if err := root.WriteFile(
			temporary.sumName,
			goSum.content,
			0o600,
		); err != nil {
			cleanupErr := temporary.cleanup(root)
			return temporaryModfile{}, fmt.Errorf(
				"write annotation tool preview sumfile: %w",
				errors.Join(err, cleanupErr),
			)
		}
	}
	return temporary, nil
}

func (temporary temporaryModfile) cleanup(root *os.Root) error {
	var result error
	for _, name := range []string{
		temporary.modName,
		temporary.sumName,
	} {
		result = errors.Join(result, removeIfExists(root, name))
	}
	return result
}

func (temporary temporaryModfile) readStates(
	root *os.Root,
) (map[string]fileChange, error) {
	mod, mode, err := readModuleFile(
		root,
		temporary.modName,
		true,
	)
	if err != nil {
		return nil, err
	}
	sum, _, err := readModuleFile(
		root,
		temporary.sumName,
		false,
	)
	if err != nil {
		return nil, err
	}
	return map[string]fileChange{
		"go.mod": {
			name:  "go.mod",
			after: mod,
			mode:  mode,
		},
		"go.sum": {
			name:  "go.sum",
			after: sum,
			mode:  0o600,
		},
	}, nil
}

func readModuleStates(
	root *os.Root,
) (map[string]fileChange, error) {
	mod, mode, err := readModuleFile(root, "go.mod", true)
	if err != nil {
		return nil, err
	}
	sum, sumMode, err := readModuleFile(root, "go.sum", false)
	if err != nil {
		return nil, err
	}
	return map[string]fileChange{
		"go.mod": {
			name:   "go.mod",
			before: mod,
			mode:   mode,
		},
		"go.sum": {
			name:   "go.sum",
			before: sum,
			mode:   sumMode,
		},
	}, nil
}

func readModuleFile(
	root *os.Root,
	name string,
	required bool,
) (fileState, fs.FileMode, error) {
	info, err := root.Stat(name)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return newFileState(false, nil), 0o600, nil
	}
	if err != nil {
		return fileState{}, 0, fmt.Errorf(
			"inspect annotation tool module file %s: %w",
			name,
			err,
		)
	}
	if !info.Mode().IsRegular() ||
		info.Size() > maximumModuleFileBytes {
		return fileState{}, 0, fmt.Errorf(
			"annotation tool module file %s must be a regular file no larger than %d bytes",
			name,
			maximumModuleFileBytes,
		)
	}
	content, err := root.ReadFile(name)
	if err != nil {
		return fileState{}, 0, fmt.Errorf(
			"read annotation tool module file %s: %w",
			name,
			err,
		)
	}
	return newFileState(true, content), info.Mode().Perm(), nil
}

func newFileState(exists bool, content []byte) fileState {
	payload := make([]byte, len(content)+1)
	if exists {
		payload[0] = 1
	}
	copy(payload[1:], content)
	return fileState{
		exists:  exists,
		content: bytes.Clone(content),
		hash:    sha256.Sum256(payload),
	}
}

func sameFileState(left, right fileState) bool {
	return left.exists == right.exists && left.hash == right.hash
}

func changedModuleFiles(
	before map[string]fileChange,
	after map[string]fileChange,
) []fileChange {
	var result []fileChange
	for _, name := range []string{"go.mod", "go.sum"} {
		change := before[name]
		change.after = after[name].after
		if change.mode == 0 {
			change.mode = after[name].mode
		}
		if !sameFileState(change.before, change.after) {
			result = append(result, change)
		}
	}
	return result
}

func moduleFileGuards(before map[string]fileChange) []fileGuard {
	return []fileGuard{
		{
			name:  "go.mod",
			state: before["go.mod"].before,
		},
		{
			name:  "go.sum",
			state: before["go.sum"].before,
		},
	}
}

func developerCommand(kind previewKind, selector string) string {
	if kind == previewTool {
		return "go get -tool " + selector
	}
	return "go get " + selector
}

func previewLabel(kind previewKind) string {
	if kind == previewTool {
		return "annotation tool"
	}
	return "module dependency"
}

func moduleAuthorizesTool(content []byte, toolPath string) bool {
	file, err := modfile.Parse("go.mod", content, nil)
	if err != nil {
		return false
	}
	for _, tool := range file.Tool {
		if tool != nil && tool.Path == toolPath {
			return true
		}
	}
	return false
}

func previewToken(preview Preview) string {
	var content []byte
	content = append(content, filepath.Clean(preview.root)...)
	content = append(content, 0)
	content = append(content, preview.kind...)
	content = append(content, 0)
	content = append(content, preview.path...)
	content = append(content, 0)
	content = append(content, preview.version...)
	for _, guard := range preview.guards {
		content = append(content, 0)
		content = append(content, guard.name...)
		content = append(content, guard.state.hash[:]...)
	}
	for _, change := range preview.changes {
		content = append(content, 0)
		content = append(content, change.name...)
		content = append(content, change.before.hash[:]...)
		content = append(content, change.after.hash[:]...)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func commandFailureDetail(
	stdout *boundedBuffer,
	stderr *boundedBuffer,
) string {
	content := strings.TrimSpace(
		strings.Join(
			[]string{stdout.String(), stderr.String()},
			"\n",
		),
	)
	if content == "" {
		return ""
	}
	return ": " + content
}

func installEnvironment(environment []string) []string {
	if environment == nil {
		environment = os.Environ()
	}
	flags := strings.Fields(environmentValue(environment, "GOFLAGS"))
	filtered := make([]string, 0, len(flags))
	for index := 0; index < len(flags); index++ {
		switch {
		case (flags[index] == "-mod" ||
			flags[index] == "-modfile") &&
			index+1 < len(flags):
			index++
		case strings.HasPrefix(flags[index], "-mod="),
			strings.HasPrefix(flags[index], "-modfile="):
		default:
			filtered = append(filtered, flags[index])
		}
	}
	return replaceEnvironment(
		environment,
		"GOFLAGS",
		strings.Join(filtered, " "),
	)
}

func environmentValue(environment []string, name string) string {
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func replaceEnvironment(
	environment []string,
	name string,
	value string,
) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if found && strings.EqualFold(key, name) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, name+"="+value)
}

type boundedBuffer struct {
	limit    int
	content  bytes.Buffer
	overflow bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (buffer *boundedBuffer) Write(content []byte) (int, error) {
	accepted := content
	remaining := buffer.limit - buffer.content.Len()
	if remaining < len(content) {
		buffer.overflow = true
		if remaining < 0 {
			remaining = 0
		}
		accepted = content[:remaining]
	}
	if len(accepted) != 0 {
		_, _ = buffer.content.Write(accepted)
	}
	return len(content), nil
}

func (buffer *boundedBuffer) String() string {
	return buffer.content.String()
}

type stagedChange struct {
	change    fileChange
	temporary string
	backup    string
	backedUp  bool
	installed bool
}

func applyChanges(
	ctx context.Context,
	root *os.Root,
	changes []fileChange,
) error {
	staged := make([]stagedChange, len(changes))
	for index, change := range changes {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, cleanupStaged(root, staged))
		}
		item, err := stageChange(root, change)
		if err != nil {
			return errors.Join(err, cleanupStaged(root, staged))
		}
		staged[index] = item
	}
	for index := range staged {
		if err := backupChange(root, &staged[index]); err != nil {
			return errors.Join(err, rollbackChanges(root, staged))
		}
	}
	for index := range staged {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, rollbackChanges(root, staged))
		}
		if err := installChange(root, &staged[index]); err != nil {
			return errors.Join(err, rollbackChanges(root, staged))
		}
	}
	return cleanupBackups(root, staged)
}

func stageChange(
	root *os.Root,
	change fileChange,
) (stagedChange, error) {
	suffix, err := randomSuffix()
	if err != nil {
		return stagedChange{}, err
	}
	result := stagedChange{
		change:    change,
		temporary: "." + change.name + ".spice-new-" + suffix,
		backup:    "." + change.name + ".spice-old-" + suffix,
	}
	if !change.after.exists {
		result.temporary = ""
		return result, nil
	}
	file, err := root.OpenFile(
		result.temporary,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		change.mode.Perm(),
	)
	if err != nil {
		return stagedChange{}, fmt.Errorf(
			"stage annotation tool module file %s: %w",
			change.name,
			err,
		)
	}
	writeErr := writeAll(file, change.after.content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		removeErr := removeIfExists(root, result.temporary)
		return stagedChange{}, fmt.Errorf(
			"stage annotation tool module file %s: %w",
			change.name,
			errors.Join(err, removeErr),
		)
	}
	return result, nil
}

func backupChange(root *os.Root, staged *stagedChange) error {
	if !staged.change.before.exists {
		if _, err := root.Lstat(staged.change.name); err == nil {
			return fmt.Errorf(
				"%w: %s was created after preview",
				ErrStalePreview,
				staged.change.name,
			)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := root.Rename(
		staged.change.name,
		staged.backup,
	); err != nil {
		return fmt.Errorf(
			"guard annotation tool module file %s: %w",
			staged.change.name,
			err,
		)
	}
	staged.backedUp = true
	state, _, err := readModuleFile(root, staged.backup, true)
	if err != nil {
		return err
	}
	if !sameFileState(state, staged.change.before) {
		return fmt.Errorf(
			"%w: %s changed while applying preview",
			ErrStalePreview,
			staged.change.name,
		)
	}
	return nil
}

func installChange(root *os.Root, staged *stagedChange) error {
	if !staged.change.after.exists {
		staged.installed = true
		return nil
	}
	if err := root.Rename(
		staged.temporary,
		staged.change.name,
	); err != nil {
		return fmt.Errorf(
			"install annotation tool module file %s: %w",
			staged.change.name,
			err,
		)
	}
	staged.installed = true
	staged.temporary = ""
	return nil
}

func rollbackChanges(root *os.Root, staged []stagedChange) error {
	var result error
	for _, item := range slices.Backward(staged) {
		if item.installed && item.change.after.exists {
			result = errors.Join(
				result,
				removeIfExists(root, item.change.name),
			)
		}
		if item.backedUp {
			result = errors.Join(
				result,
				root.Rename(item.backup, item.change.name),
			)
		}
		if item.temporary != "" {
			result = errors.Join(
				result,
				removeIfExists(root, item.temporary),
			)
		}
	}
	return result
}

func cleanupStaged(root *os.Root, staged []stagedChange) error {
	var result error
	for _, item := range staged {
		if item.temporary != "" {
			result = errors.Join(
				result,
				removeIfExists(root, item.temporary),
			)
		}
	}
	return result
}

func cleanupBackups(root *os.Root, staged []stagedChange) error {
	var result error
	for _, item := range staged {
		if item.backedUp {
			result = errors.Join(
				result,
				removeIfExists(root, item.backup),
			)
		}
	}
	return result
}

func removeIfExists(root *os.Root, name string) error {
	if name == "" {
		return nil
	}
	err := root.Remove(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func randomSuffix() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf(
			"create annotation tool module-file nonce: %w",
			err,
		)
	}
	return hex.EncodeToString(value[:]), nil
}

func writeAll(writer io.Writer, content []byte) error {
	for len(content) != 0 {
		written, err := writer.Write(content)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func moduleDiff(changes []fileChange) string {
	ordered := append([]fileChange(nil), changes...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].name < ordered[right].name
	})
	var result strings.Builder
	for _, change := range ordered {
		result.WriteString(unifiedFileDiff(change))
	}
	return result.String()
}

func unifiedFileDiff(change fileChange) string {
	before := splitLines(change.before.content)
	after := splitLines(change.after.content)
	prefix := commonPrefix(before, after)
	suffix := commonSuffix(before[prefix:], after[prefix:])
	beforeEnd := len(before) - suffix
	afterEnd := len(after) - suffix
	var result strings.Builder
	oldName := change.name
	if !change.before.exists {
		oldName = "/dev/null"
	}
	newName := change.name
	if !change.after.exists {
		newName = "/dev/null"
	}
	fmt.Fprintf(&result, "--- %s\n+++ %s\n", oldName, newName)
	fmt.Fprintf(
		&result,
		"@@ -%d,%d +%d,%d @@\n",
		unifiedRangeStart(prefix, beforeEnd-prefix),
		beforeEnd-prefix,
		unifiedRangeStart(prefix, afterEnd-prefix),
		afterEnd-prefix,
	)
	for _, line := range before[prefix:beforeEnd] {
		result.WriteByte('-')
		result.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			result.WriteByte('\n')
			result.WriteString("\\ No newline at end of file\n")
		}
	}
	for _, line := range after[prefix:afterEnd] {
		result.WriteByte('+')
		result.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			result.WriteByte('\n')
			result.WriteString("\\ No newline at end of file\n")
		}
	}
	return result.String()
}

func unifiedRangeStart(prefix, count int) int {
	if count == 0 {
		return prefix
	}
	return prefix + 1
}

func splitLines(content []byte) []string {
	if len(content) == 0 {
		return []string{}
	}
	result := make([]string, 0, bytes.Count(content, []byte{'\n'})+1)
	for len(content) != 0 {
		index := bytes.IndexByte(content, '\n')
		if index < 0 {
			result = append(result, string(content))
			break
		}
		result = append(result, string(content[:index+1]))
		content = content[index+1:]
	}
	return result
}

func commonPrefix(left, right []string) int {
	length := min(len(left), len(right))
	index := 0
	for index < length && left[index] == right[index] {
		index++
	}
	return index
}

func commonSuffix(left, right []string) int {
	length := min(len(left), len(right))
	index := 0
	for index < length &&
		left[len(left)-index-1] == right[len(right)-index-1] {
		index++
	}
	return index
}
