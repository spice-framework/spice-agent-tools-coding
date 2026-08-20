package process

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

var (
	errExecutableDigestMismatch   = errors.New("executable digest mismatch")
	errExecutableIdentityMismatch = errors.New("executable identity mismatch")
	errExecutableLeaseClosed      = errors.New("executable verification lease is closed")
)

// VerificationOperation identifies one secret-safe verified-executable step.
type VerificationOperation string

const (
	VerificationOperationValidate    VerificationOperation = "validate"
	VerificationOperationOpen        VerificationOperation = "open"
	VerificationOperationInspect     VerificationOperation = "inspect"
	VerificationOperationHash        VerificationOperation = "hash"
	VerificationOperationDuplicate   VerificationOperation = "duplicate"
	VerificationOperationMaterialize VerificationOperation = "materialize"
	VerificationOperationRecheck     VerificationOperation = "recheck"
	VerificationOperationClose       VerificationOperation = "close"
)

// VerificationError preserves cancellation and platform error identity for
// deliberate inspection. Its formatted and serialized forms never include a
// path, digest, environment entry, file identity, or platform error text.
type VerificationError struct {
	operation VerificationOperation
	cause     error
}

func (failure *VerificationError) Error() string {
	if failure == nil || !validVerificationOperation(failure.operation) {
		return "executable verification failed"
	}
	return "executable verification failed: " + string(failure.operation)
}

func (failure *VerificationError) Operation() VerificationOperation {
	if failure == nil {
		return ""
	}
	return failure.operation
}

func (failure *VerificationError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func (failure *VerificationError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, failure.Error())
}

func (failure *VerificationError) MarshalJSON() ([]byte, error) {
	return json.Marshal(failure.Error())
}

func (failure *VerificationError) LogValue() slog.Value {
	return slog.StringValue(failure.Error())
}

// ExecutableLease holds one verified executable object open across process
// launch. The lease binds an immutable canonical path and expected digest to a
// platform file identity. Close is idempotent and concurrency-safe.
type ExecutableLease struct {
	mu       sync.Mutex
	file     *os.File
	identity executableFileIdentity
	path     string
	digest   SHA256
}

// VerifyExecutable opens and hashes one canonical absolute executable before
// any child is started. It performs no PATH lookup, shell invocation, network
// access, or dependency resolution.
func VerifyExecutable(
	ctx context.Context,
	path string,
	expected SHA256,
) (*ExecutableLease, error) {
	if ctx == nil {
		return nil, verificationFailure(VerificationOperationValidate, errors.New("nil context"))
	}
	if err := validatePath("executable", path); err != nil {
		return nil, verificationFailure(VerificationOperationValidate, err)
	}
	if err := expected.Validate(); err != nil {
		return nil, verificationFailure(VerificationOperationValidate, err)
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, verificationFailure(VerificationOperationOpen, cause)
	}
	file, identity, err := openExecutableFile(path)
	if err != nil {
		return nil, verificationFailure(VerificationOperationOpen, err)
	}
	lease := &ExecutableLease{file: file, identity: identity, path: path, digest: expected}
	digest, err := hashExecutable(ctx, file)
	if err != nil {
		_ = file.Close()
		return nil, verificationFailure(VerificationOperationHash, err)
	}
	if !digest.equal(expected) {
		_ = file.Close()
		return nil, verificationFailure(VerificationOperationHash, errExecutableDigestMismatch)
	}
	return lease, nil
}

// Path returns the immutable canonical path whose file object was verified.
func (lease *ExecutableLease) Path() string {
	if lease == nil {
		return ""
	}
	return lease.path
}

// Digest returns the immutable expected executable digest.
func (lease *ExecutableLease) Digest() SHA256 {
	if lease == nil {
		return SHA256{}
	}
	return lease.digest
}

// ValidateSpec proves that a child specification selects the leased path. It
// deliberately does not relax or reconstruct the independently validated Spec.
func (lease *ExecutableLease) ValidateSpec(spec Spec) error {
	if lease == nil {
		return verificationFailure(VerificationOperationValidate, errExecutableLeaseClosed)
	}
	if err := spec.Validate(); err != nil {
		return verificationFailure(VerificationOperationValidate, err)
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.file == nil {
		return verificationFailure(VerificationOperationValidate, errExecutableLeaseClosed)
	}
	if spec.Executable() != lease.path {
		return verificationFailure(VerificationOperationValidate, errExecutableIdentityMismatch)
	}
	return nil
}

// DuplicateForLaunch returns a caller-owned duplicate of the verified file
// object. A trusted native launcher uses this handle for exact-image execution
// on platforms that support descriptor-backed exec. The lease remains owned by
// its caller and must outlive process containment.
func (lease *ExecutableLease) DuplicateForLaunch() (*os.File, error) {
	if lease == nil {
		return nil, verificationFailure(VerificationOperationDuplicate, errExecutableLeaseClosed)
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.file == nil {
		return nil, verificationFailure(VerificationOperationDuplicate, errExecutableLeaseClosed)
	}
	duplicate, err := duplicateExecutableFile(lease.file)
	if err != nil {
		return nil, verificationFailure(VerificationOperationDuplicate, err)
	}
	return duplicate, nil
}

// Recheck reopens the configured path and proves it still resolves to the same
// platform file identity and content digest. Native launchers use it before a
// suspended child can execute where descriptor-backed exec is unavailable;
// callers may also retain it as defense-in-depth after launch.
func (lease *ExecutableLease) Recheck(ctx context.Context) error {
	if ctx == nil {
		return verificationFailure(VerificationOperationRecheck, errors.New("nil context"))
	}
	if lease == nil {
		return verificationFailure(VerificationOperationRecheck, errExecutableLeaseClosed)
	}
	if cause := context.Cause(ctx); cause != nil {
		return verificationFailure(VerificationOperationRecheck, cause)
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.file == nil {
		return verificationFailure(VerificationOperationRecheck, errExecutableLeaseClosed)
	}
	current, identity, err := openExecutableFile(lease.path)
	if err != nil {
		return verificationFailure(VerificationOperationRecheck, err)
	}
	defer func() { _ = current.Close() }()
	if !lease.identity.equal(identity) {
		return verificationFailure(VerificationOperationRecheck, errExecutableIdentityMismatch)
	}
	digest, err := hashExecutable(ctx, current)
	if err != nil {
		return verificationFailure(VerificationOperationRecheck, err)
	}
	if !digest.equal(lease.digest) {
		return verificationFailure(VerificationOperationRecheck, errExecutableDigestMismatch)
	}
	return nil
}

// Close releases the held executable object. A nil or already-closed lease is
// harmless. Callers must not close a live child's lease before containment is
// proved.
func (lease *ExecutableLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.file == nil {
		return nil
	}
	err := lease.file.Close()
	lease.file = nil
	if err != nil {
		return verificationFailure(VerificationOperationClose, err)
	}
	return nil
}

func hashExecutable(ctx context.Context, file *os.File) (SHA256, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return SHA256{}, err
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	for {
		if cause := context.Cause(ctx); cause != nil {
			return SHA256{}, cause
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return SHA256{}, readErr
		}
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return newSHA256(sum), nil
}

func verificationFailure(operation VerificationOperation, cause error) error {
	if cause == nil {
		return nil
	}
	if !validVerificationOperation(operation) {
		operation = ""
	}
	return &VerificationError{operation: operation, cause: cause}
}

func validVerificationOperation(operation VerificationOperation) bool {
	switch operation {
	case VerificationOperationValidate, VerificationOperationOpen, VerificationOperationInspect,
		VerificationOperationHash, VerificationOperationDuplicate, VerificationOperationMaterialize,
		VerificationOperationRecheck,
		VerificationOperationClose:
		return true
	default:
		return false
	}
}

func (*ExecutableLease) String() string   { return "process.ExecutableLease([REDACTED])" }
func (*ExecutableLease) GoString() string { return "process.ExecutableLease([REDACTED])" }
func (lease *ExecutableLease) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, lease.String())
}

func (lease *ExecutableLease) MarshalJSON() ([]byte, error) {
	return json.Marshal(lease.String())
}
func (lease *ExecutableLease) LogValue() slog.Value { return slog.StringValue(lease.String()) }
