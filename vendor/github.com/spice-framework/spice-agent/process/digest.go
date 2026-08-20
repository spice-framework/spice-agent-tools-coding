package process

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SHA256 is an immutable exact executable-content identity. Its bytes remain
// private so callers cannot mutate a validated digest through shared storage.
type SHA256 struct{ value [sha256.Size]byte }

// DigestProblem classifies a secret-safe SHA-256 validation failure.
type DigestProblem string

const (
	DigestProblemMalformed DigestProblem = "malformed"
	DigestProblemZero      DigestProblem = "zero"
)

// DigestError reports an invalid executable digest without retaining or
// formatting the caller-supplied value.
type DigestError struct{ problem DigestProblem }

func (failure *DigestError) Error() string {
	if failure == nil || failure.problem == "" {
		return "invalid executable sha256"
	}
	return "invalid executable sha256: " + string(failure.problem)
}

func (failure *DigestError) Problem() DigestProblem {
	if failure == nil {
		return ""
	}
	return failure.problem
}

func (failure *DigestError) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, failure.Error())
}

func (failure *DigestError) MarshalJSON() ([]byte, error) {
	return json.Marshal(failure.Error())
}

// ParseSHA256 accepts exactly 64 lowercase hexadecimal characters. Parsing
// does not accept a zero digest as a usable pin; callers must also use Validate.
func ParseSHA256(value string) (SHA256, error) {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return SHA256{}, &DigestError{problem: DigestProblemMalformed}
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return SHA256{}, &DigestError{problem: DigestProblemMalformed}
	}
	var result SHA256
	copy(result.value[:], decoded)
	return result, nil
}

// Validate rejects the zero value, which cannot authorize an executable.
func (digest SHA256) Validate() error {
	if digest.value == [sha256.Size]byte{} {
		return &DigestError{problem: DigestProblemZero}
	}
	return nil
}

// String returns the canonical lowercase hexadecimal digest.
func (digest SHA256) String() string { return hex.EncodeToString(digest.value[:]) }

func newSHA256(value [sha256.Size]byte) SHA256 { return SHA256{value: value} }

func (digest SHA256) equal(other SHA256) bool { return digest.value == other.value }
