package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

const materializedExecutableDirectoryPattern = "spice-agent-verified-*"

type materializedDestinationOpener func(string, int, os.FileMode) (*os.File, error)

// MaterializedExecutable is a private, digest-reverified executable snapshot.
// It supports platforms such as Darwin that cannot execute an already-open
// descriptor. Close keeps its verification lease until the caller has proved
// child containment, then removes exactly the snapshot and its private
// directory.
type MaterializedExecutable struct {
	mu        sync.Mutex
	lease     *ExecutableLease
	directory string
	path      string
}

// MaterializeForLaunch copies bytes only from the verified file object into a
// newly created process-owned directory, synchronizes and closes the writer,
// and verifies the exact expected digest again before returning. It performs no
// PATH lookup and never reopens the configured source pathname.
func (lease *ExecutableLease) MaterializeForLaunch(ctx context.Context) (*MaterializedExecutable, error) {
	return lease.materializeForLaunch(ctx, "", os.OpenFile)
}

func (lease *ExecutableLease) materializeForLaunch(
	ctx context.Context,
	parent string,
	openDestination materializedDestinationOpener,
) (_ *MaterializedExecutable, resultErr error) {
	if ctx == nil || openDestination == nil {
		return nil, verificationFailure(VerificationOperationMaterialize, errors.New("invalid materialization input"))
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, verificationFailure(VerificationOperationMaterialize, cause)
	}
	source, err := lease.DuplicateForLaunch()
	if err != nil {
		return nil, verificationFailure(VerificationOperationMaterialize, err)
	}
	sourceOpen := true
	defer func() {
		if sourceOpen {
			closeErr := source.Close()
			if closeErr != nil {
				resultErr = verificationFailure(
					VerificationOperationClose,
					errors.Join(resultErr, closeErr),
				)
			}
		}
	}()

	directory, err := os.MkdirTemp(parent, materializedExecutableDirectoryPattern)
	if err != nil {
		return nil, verificationFailure(VerificationOperationMaterialize, err)
	}
	path := filepath.Join(directory, materializedExecutableName())
	keep := false
	defer func() {
		if keep {
			return
		}
		cleanupErr := cleanupMaterializedExecutable(path, directory)
		if cleanupErr != nil {
			resultErr = verificationFailure(
				VerificationOperationClose,
				errors.Join(resultErr, cleanupErr),
			)
		}
	}()
	if err = os.Chmod(directory, 0o700); err != nil { // #nosec G302 -- a private directory requires owner search permission.
		return nil, verificationFailure(VerificationOperationMaterialize, err)
	}
	destination, err := openDestination(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, verificationFailure(VerificationOperationMaterialize, err)
	}
	if err = copyExecutable(ctx, destination, source); err == nil {
		err = destination.Sync()
	}
	if err == nil {
		err = destination.Chmod(0o500)
	}
	err = errors.Join(err, destination.Close())
	if err != nil {
		return nil, verificationFailure(VerificationOperationMaterialize, err)
	}
	if err = source.Close(); err != nil {
		return nil, verificationFailure(VerificationOperationClose, err)
	}
	sourceOpen = false
	snapshotLease, err := VerifyExecutable(ctx, path, lease.Digest())
	if err != nil {
		return nil, verificationFailure(VerificationOperationMaterialize, err)
	}
	keep = true
	return &MaterializedExecutable{
		lease: snapshotLease, directory: directory, path: path,
	}, nil
}

// Path returns the absolute private path selected for launch. It is intended
// only as the executable argument of a trusted VerifiedLauncher.
func (executable *MaterializedExecutable) Path() string {
	if executable == nil {
		return ""
	}
	executable.mu.Lock()
	defer executable.mu.Unlock()
	return executable.path
}

// Recheck proves that the private launch path still identifies the exact
// materialized object and digest.
func (executable *MaterializedExecutable) Recheck(ctx context.Context) error {
	if executable == nil {
		return verificationFailure(VerificationOperationRecheck, errExecutableLeaseClosed)
	}
	executable.mu.Lock()
	lease := executable.lease
	executable.mu.Unlock()
	if lease == nil {
		return verificationFailure(VerificationOperationRecheck, errExecutableLeaseClosed)
	}
	return lease.Recheck(ctx)
}

// Close releases the materialized lease, removes exactly its private file, and
// removes the now-empty private directory. It is idempotent and refuses to
// recursively delete unexpected contents.
func (executable *MaterializedExecutable) Close() error {
	if executable == nil {
		return nil
	}
	executable.mu.Lock()
	defer executable.mu.Unlock()
	if executable.lease == nil {
		return nil
	}
	lease, path, directory := executable.lease, executable.path, executable.directory
	err := errors.Join(lease.Close(), cleanupMaterializedExecutable(path, directory))
	if err == nil {
		executable.lease, executable.path, executable.directory = nil, "", ""
	}
	return verificationFailure(
		VerificationOperationClose,
		err,
	)
}

func copyExecutable(ctx context.Context, destination, source *os.File) error {
	buffer := make([]byte, 64<<10)
	var offset int64
	for {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		count, readErr := source.ReadAt(buffer, offset)
		if count > 0 {
			written, writeErr := destination.Write(buffer[:count])
			if writeErr != nil {
				return writeErr
			}
			if written != count {
				return io.ErrShortWrite
			}
			offset += int64(count)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func cleanupMaterializedExecutable(path, directory string) error {
	var failures []error
	if path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	if directory != "" {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func materializedExecutableName() string {
	if runtime.GOOS == "windows" {
		return "executable.exe"
	}
	return "executable"
}

func (*MaterializedExecutable) String() string {
	return "process.MaterializedExecutable([REDACTED])"
}

func (*MaterializedExecutable) GoString() string {
	return "process.MaterializedExecutable([REDACTED])"
}

func (executable *MaterializedExecutable) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, executable.String())
}

func (executable *MaterializedExecutable) MarshalJSON() ([]byte, error) {
	return json.Marshal(executable.String())
}

func (executable *MaterializedExecutable) LogValue() slog.Value {
	return slog.StringValue(executable.String())
}
