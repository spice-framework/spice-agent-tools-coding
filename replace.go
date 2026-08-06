package coding

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spice-framework/spice-agent/tool"
)

const replaceInputSchema = `{"type":"object","additionalProperties":false,"required":["path","content"],"properties":{"path":{"type":"string","minLength":1},"content":{"type":"string"},"expected_sha256":{"type":"string"},"create":{"type":"boolean"}}}`

const (
	maximumRenameAttempts = 64
	renameRetryDelay      = 25 * time.Millisecond
)

type replaceTool struct {
	config       Config
	definition   tool.Definition
	beforeCommit func()
	syncParent   func(*os.Root, string) error
	renameTarget func(*os.Root, string, string) error
	commitLease  chan struct{}
}

type replaceArguments struct {
	Path           string `json:"path"`
	Content        string `json:"content"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	Create         bool   `json:"create,omitempty"`
}

type replaceContent struct {
	OK               bool   `json:"ok"`
	Path             string `json:"path"`
	Bytes            int    `json:"bytes"`
	SHA256           string `json:"sha256"`
	Created          bool   `json:"created"`
	Committed        bool   `json:"committed"`
	Durable          bool   `json:"durable"`
	TemporaryCleaned bool   `json:"temporary_cleaned"`
}

// NewReplace constructs the exact Spice Agent stale-protected replace/write
// tool binding without touching the configured worktree.
func NewReplace(config Config) (tool.Tool, error) {
	normalized, err := validatedConfig(config)
	if err != nil {
		return nil, err
	}
	definition, err := tool.NewDefinition(
		"replace",
		"Atomically create or stale-protected replace one bounded worktree file.",
		json.RawMessage(replaceInputSchema),
		tool.CapabilityFilesystemRead,
		tool.CapabilityFilesystemWrite,
	)
	if err != nil {
		return nil, err
	}
	return &replaceTool{
		config: normalized, definition: definition, syncParent: syncDirectory,
		renameTarget: renameWithinRoot, commitLease: make(chan struct{}, 1),
	}, nil
}

func (replacer *replaceTool) Definition() tool.Definition { return replacer.definition.Clone() }

func (replacer *replaceTool) Execute(ctx context.Context, call tool.Call, reporter tool.Reporter) tool.Result {
	if err := validateCall(call, replacer.definition.Name()); err != nil {
		return failureResult(call.ID(), err)
	}
	if err := contextFailure(ctx); err != nil {
		return failureResult(call.ID(), err)
	}
	var arguments replaceArguments
	if err := decodeArguments(call.Arguments(), &arguments); err != nil {
		return failureResult(call.ID(), err)
	}
	if int64(len(arguments.Content)) > replacer.config.MaxWriteBytes {
		return failureResult(call.ID(), invalidArguments("replacement content exceeds the configured write bound"))
	}
	if err := validateReplaceMode(arguments); err != nil {
		return failureResult(call.ID(), err)
	}
	if err := reportProgress(ctx, reporter, call.ID(), "preparing atomic worktree update"); err != nil {
		return failureResult(call.ID(), err)
	}
	select {
	case replacer.commitLease <- struct{}{}:
		defer func() { <-replacer.commitLease }()
	case <-ctx.Done():
		return failureResult(call.ID(), executionFailure("cancelled", "tool operation was cancelled"))
	}
	content, problem, err := replacer.replace(ctx, arguments)
	if err != nil {
		return failureResult(call.ID(), err)
	}
	return replaceResult(call.ID(), content, problem)
}

func validateReplaceMode(arguments replaceArguments) error {
	if arguments.Create {
		if arguments.ExpectedSHA256 != "" {
			return invalidArguments("create mode must not include expected_sha256")
		}
		return nil
	}
	if len(arguments.ExpectedSHA256) != sha256.Size*2 {
		return invalidArguments("replace mode requires a 64-character expected_sha256")
	}
	if _, err := hex.DecodeString(arguments.ExpectedSHA256); err != nil {
		return invalidArguments("expected_sha256 must be lowercase hexadecimal")
	}
	if arguments.ExpectedSHA256 != strings.ToLower(arguments.ExpectedSHA256) {
		return invalidArguments("expected_sha256 must be lowercase hexadecimal")
	}
	return nil
}

func (replacer *replaceTool) replace(
	ctx context.Context,
	arguments replaceArguments,
) (replaceContent, string, error) {
	path, err := parseRelativePath(arguments.Path, false)
	if err != nil {
		return replaceContent{}, "", err
	}
	workspace, err := openWorktree(replacer.config.Root)
	if err != nil {
		return replaceContent{}, "", err
	}
	defer workspace.Close() //nolint:errcheck // Operation errors take precedence over close-only root release.
	mode, err := replacer.preflight(workspace, path, arguments)
	if err != nil {
		return replaceContent{}, "", err
	}
	temporary, err := writeTemporary(ctx, workspace, filepath.Dir(path.native), []byte(arguments.Content), mode)
	if err != nil {
		return replaceContent{}, "", err
	}
	defer func() {
		if temporary != "" {
			removeBestEffort(workspace, temporary)
		}
	}()
	if contextErr := contextFailure(ctx); contextErr != nil {
		return replaceContent{}, "", contextErr
	}
	if replacer.beforeCommit != nil {
		replacer.beforeCommit()
	}
	if arguments.Create {
		err = commitCreate(workspace, temporary, path.native)
	} else {
		err = replacer.commitReplace(ctx, workspace, temporary, path, arguments.ExpectedSHA256)
	}
	if err != nil {
		return replaceContent{}, "", err
	}
	temporaryCleaned := true
	if arguments.Create {
		if err := workspace.Remove(temporary); err != nil {
			temporaryCleaned = false
		} else {
			temporary = ""
		}
	} else {
		temporary = ""
	}
	digest := sha256.Sum256([]byte(arguments.Content))
	content := replaceContent{
		OK: true, Path: path.display, Bytes: len(arguments.Content),
		SHA256: hex.EncodeToString(digest[:]), Created: arguments.Create, Committed: true, Durable: true,
		TemporaryCleaned: temporaryCleaned,
	}
	fileSyncErr := syncCommittedFile(workspace, path.native)
	parentSyncErr := replacer.syncParent(workspace, filepath.Dir(path.native))
	if err := errors.Join(fileSyncErr, parentSyncErr); err != nil {
		content.OK = false
		content.Durable = false
		if !temporaryCleaned {
			return content, "worktree update committed but durability and temporary cleanup could not be confirmed", nil
		}
		return content, "worktree update committed but durability could not be confirmed", nil
	}
	if !temporaryCleaned {
		content.OK = false
		return content, "worktree update committed but temporary cleanup could not be confirmed", nil
	}
	return content, "", nil
}

func replaceResult(callID tool.CallID, content replaceContent, problem string) tool.Result {
	encoded, err := json.Marshal(content)
	if err != nil {
		return failureResult(callID, executionFailure("internal_contract", "replace result could not be encoded"))
	}
	var result tool.Result
	if problem == "" {
		result, err = tool.NewResult(callID, encoded)
	} else {
		result, err = tool.NewErrorResult(callID, encoded, problem)
	}
	if err != nil {
		return failureResult(callID, executionFailure("internal_contract", "replace result could not be encoded"))
	}
	return result
}

func (replacer *replaceTool) preflight(workspace *os.Root, path relativePath, arguments replaceArguments) (os.FileMode, error) {
	info, err := workspace.Lstat(path.native)
	if arguments.Create {
		if err == nil {
			return 0, executionFailure("already_exists", "create target already exists")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, executionFailure("path_unavailable", "create target is unavailable or escapes the configured worktree")
		}
		return 0o600, nil
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, executionFailure("not_found", "replace target does not exist")
		}
		return 0, executionFailure("path_unavailable", "replace target is unavailable or escapes the configured worktree")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, executionFailure("not_regular", "replace target must be a regular non-symbolic-link file")
	}
	digest, err := boundedDigest(workspace, path.native, replacer.config.MaxWriteBytes)
	if err != nil {
		return 0, err
	}
	if !bytes.Equal([]byte(digest), []byte(arguments.ExpectedSHA256)) {
		return 0, executionFailure("stale", "replace target changed since it was read")
	}
	return info.Mode().Perm(), nil
}

func (replacer *replaceTool) commitReplace(
	ctx context.Context,
	workspace *os.Root,
	temporary string,
	path relativePath,
	expected string,
) error {
	for attempt := range maximumRenameAttempts {
		if err := contextFailure(ctx); err != nil {
			return err
		}
		digest, err := boundedDigest(workspace, path.native, replacer.config.MaxWriteBytes)
		if err != nil {
			return err
		}
		if !bytes.Equal([]byte(digest), []byte(expected)) {
			return executionFailure("stale", "replace target changed before the atomic commit")
		}
		info, err := workspace.Lstat(path.native)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return executionFailure("stale", "replace target changed before the atomic commit")
		}
		err = replacer.renameTarget(workspace, temporary, path.native)
		if err == nil {
			return nil
		}
		if !isRetryableRenameError(err) || attempt == maximumRenameAttempts-1 {
			return executionFailure("replace_failed", "atomic replacement could not be committed")
		}
		if err := waitForRenameRetry(ctx); err != nil {
			return err
		}
	}
	return executionFailure("replace_failed", "atomic replacement could not be committed")
}

func waitForRenameRetry(ctx context.Context) error {
	timer := time.NewTimer(renameRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return executionFailure("cancelled", "tool operation was cancelled")
	case <-timer.C:
		return nil
	}
}

func commitCreate(workspace *os.Root, temporary, target string) error {
	if _, err := workspace.Lstat(target); err == nil {
		return executionFailure("already_exists", "create target appeared before the atomic commit")
	} else if !errors.Is(err, os.ErrNotExist) {
		return executionFailure("path_unavailable", "create target is unavailable or escapes the configured worktree")
	}
	if err := workspace.Link(temporary, target); err != nil {
		if _, statErr := workspace.Lstat(target); statErr == nil {
			return executionFailure("already_exists", "create target appeared before the atomic commit")
		}
		return executionFailure("create_failed", "atomic file creation is unavailable on this filesystem")
	}
	return nil
}

func boundedDigest(workspace *os.Root, name string, maximum int64) (string, error) {
	file, err := workspace.Open(name)
	if err != nil {
		return "", executionFailure("read_failed", "replace target could not be read")
	}
	defer file.Close() //nolint:errcheck // Read-only close cannot alter the digest.
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", executionFailure("not_regular", "replace target must be a regular file")
	}
	if info.Size() > maximum {
		return "", executionFailure("file_too_large", "replace target exceeds the configured write bound")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil {
		return "", executionFailure("read_failed", "replace target could not be read")
	}
	if written > maximum {
		return "", executionFailure("file_too_large", "replace target exceeds the configured write bound")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeTemporary(ctx context.Context, workspace *os.Root, parent string, content []byte, mode os.FileMode) (string, error) {
	for range 16 {
		name, err := temporaryName(parent)
		if err != nil {
			return "", executionFailure("temporary_failed", "atomic temporary file name could not be created")
		}
		file, err := workspace.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", executionFailure("temporary_failed", "atomic temporary file could not be created")
		}
		if err := writeAndSync(ctx, file, content); err != nil {
			removeBestEffort(workspace, name)
			return "", err
		}
		return name, nil
	}
	return "", executionFailure("temporary_failed", "unique atomic temporary file could not be created")
}

func temporaryName(parent string) (string, error) {
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return "", err
	}
	return filepath.Join(parent, ".spice-replace-"+hex.EncodeToString(identity[:])+".tmp"), nil
}

func writeAndSync(ctx context.Context, file *os.File, content []byte) error {
	if err := contextFailure(ctx); err != nil {
		closeBestEffort(file)
		return err
	}
	if _, err := file.Write(content); err != nil {
		closeBestEffort(file)
		return executionFailure("write_failed", "atomic temporary file could not be written")
	}
	if err := file.Sync(); err != nil {
		closeBestEffort(file)
		return executionFailure("sync_failed", "atomic temporary file could not be synchronized")
	}
	if err := file.Close(); err != nil {
		return executionFailure("close_failed", "atomic temporary file could not be closed")
	}
	return contextFailure(ctx)
}

func syncCommittedFile(workspace *os.Root, name string) error {
	file, err := workspace.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck // Sync error is the durability signal.
	return file.Sync()
}

var _ tool.Tool = (*replaceTool)(nil)
