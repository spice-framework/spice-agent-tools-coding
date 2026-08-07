package process

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/spice-framework/spice-agent/tool"
)

const (
	// MaximumArguments bounds the number of discrete child arguments.
	MaximumArguments = 4096
	// MaximumEnvironment bounds the number of exact child environment entries.
	MaximumEnvironment = 4096
	// MaximumCapabilities bounds security-relevant capability metadata.
	MaximumCapabilities = 128
	// MaximumValueBytes bounds one path, argument, or environment entry.
	MaximumValueBytes = 1 << 20
	// MaximumSpecBytes bounds all copied string data in one specification.
	MaximumSpecBytes = 4 << 20
)

// SpecProblem classifies one secret-safe specification validation failure.
type SpecProblem string

const (
	ProblemRequired          SpecProblem = "required"
	ProblemNotAbsolute       SpecProblem = "not_absolute"
	ProblemNotCanonical      SpecProblem = "not_canonical"
	ProblemInvalidUTF8       SpecProblem = "invalid_utf8"
	ProblemContainsNUL       SpecProblem = "contains_nul"
	ProblemMalformed         SpecProblem = "malformed"
	ProblemDuplicate         SpecProblem = "duplicate"
	ProblemTooMany           SpecProblem = "too_many"
	ProblemTooLarge          SpecProblem = "too_large"
	ProblemMissingCapability SpecProblem = "missing_capability"
)

// SpecError identifies a field and optional element without including its
// potentially sensitive value. Index returns -1 for a scalar field.
type SpecError struct {
	field   string
	index   int
	problem SpecProblem
}

func (failure *SpecError) Error() string {
	if failure == nil {
		return "invalid process specification"
	}
	if failure.index >= 0 {
		return fmt.Sprintf(
			"invalid process specification: %s[%d] is %s",
			failure.field,
			failure.index,
			failure.problem,
		)
	}
	return fmt.Sprintf("invalid process specification: %s is %s", failure.field, failure.problem)
}

func (failure *SpecError) Field() string        { return failure.field }
func (failure *SpecError) Index() int           { return failure.index }
func (failure *SpecError) Problem() SpecProblem { return failure.problem }
func (failure *SpecError) MarshalJSON() ([]byte, error) {
	return json.Marshal(failure.Error())
}

// Config is copied and validated by NewSpec. Arguments exclude argv[0].
// Environment entries use the ordinary name=value representation. An empty
// environment is explicit; all three streams must be non-nil.
type Config struct {
	Executable       string
	Arguments        []string
	WorkingDirectory string
	Environment      []string
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	Capabilities     []tool.Capability
}

// Spec is immutable process intent. It performs no filesystem access and does
// not establish a permission or containment boundary. Capability metadata is
// declarative input for injected policy decorators.
type Spec struct {
	executable       string
	arguments        []string
	workingDirectory string
	environment      []string
	stdin            io.Reader
	stdout           io.Writer
	stderr           io.Writer
	capabilities     []tool.Capability
}

// NewSpec validates and defensively copies one process specification.
func NewSpec(config Config) (Spec, error) {
	if err := validatePath("executable", config.Executable); err != nil {
		return Spec{}, err
	}
	if err := validatePath("working_directory", config.WorkingDirectory); err != nil {
		return Spec{}, err
	}
	if config.Stdin == nil {
		return Spec{}, specFailure("stdin", -1, ProblemRequired)
	}
	if config.Stdout == nil {
		return Spec{}, specFailure("stdout", -1, ProblemRequired)
	}
	if config.Stderr == nil {
		return Spec{}, specFailure("stderr", -1, ProblemRequired)
	}

	total := len(config.Executable) + len(config.WorkingDirectory)
	arguments, size, err := copyValues("arguments", config.Arguments, MaximumArguments)
	if err != nil {
		return Spec{}, err
	}
	total += size
	environment, size, err := copyEnvironment(config.Environment)
	if err != nil {
		return Spec{}, err
	}
	total += size
	capabilities, size, err := copyCapabilities(config.Capabilities)
	if err != nil {
		return Spec{}, err
	}
	total += size
	if total > MaximumSpecBytes {
		return Spec{}, specFailure("spec", -1, ProblemTooLarge)
	}

	return Spec{
		executable: config.Executable, arguments: arguments,
		workingDirectory: config.WorkingDirectory, environment: environment,
		stdin: config.Stdin, stdout: config.Stdout, stderr: config.Stderr,
		capabilities: capabilities,
	}, nil
}

// Validate rejects a zero or corrupted specification without performing I/O.
func (spec Spec) Validate() error {
	_, err := NewSpec(Config{
		Executable: spec.executable, Arguments: spec.arguments,
		WorkingDirectory: spec.workingDirectory, Environment: spec.environment,
		Stdin: spec.stdin, Stdout: spec.stdout, Stderr: spec.stderr,
		Capabilities: spec.capabilities,
	})
	return err
}

func (spec Spec) Executable() string              { return spec.executable }
func (spec Spec) Arguments() []string             { return slices.Clone(spec.arguments) }
func (spec Spec) WorkingDirectory() string        { return spec.workingDirectory }
func (spec Spec) Environment() []string           { return slices.Clone(spec.environment) }
func (spec Spec) Stdin() io.Reader                { return spec.stdin }
func (spec Spec) Stdout() io.Writer               { return spec.stdout }
func (spec Spec) Stderr() io.Writer               { return spec.stderr }
func (spec Spec) Capabilities() []tool.Capability { return slices.Clone(spec.capabilities) }

// Clone returns an independently backed immutable value.
func (spec Spec) Clone() Spec {
	spec.arguments = slices.Clone(spec.arguments)
	spec.environment = slices.Clone(spec.environment)
	spec.capabilities = slices.Clone(spec.capabilities)
	return spec
}

// String prevents command arguments, environment values, paths, or stream
// implementations from leaking through incidental formatting.
func (Spec) String() string   { return "process.Spec([REDACTED])" }
func (Spec) GoString() string { return "process.Spec([REDACTED])" }
func (Spec) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "process.Spec([REDACTED])")
}

func (Spec) MarshalJSON() ([]byte, error) {
	return json.Marshal("process.Spec([REDACTED])")
}

func validatePath(field, value string) error {
	return validatePathWith(field, value, specFailure)
}

type validationFailure func(string, int, SpecProblem) error

func validatePathWith(field, value string, fail validationFailure) error {
	if value == "" {
		return fail(field, -1, ProblemRequired)
	}
	if err := validateValueWith(field, -1, value, fail); err != nil {
		return err
	}
	if !filepath.IsAbs(value) {
		return fail(field, -1, ProblemNotAbsolute)
	}
	if filepath.Clean(value) != value {
		return fail(field, -1, ProblemNotCanonical)
	}
	return nil
}

func copyValues(field string, values []string, maximum int) ([]string, int, error) {
	return copyValuesWith(field, values, maximum, specFailure)
}

func copyValuesWith(
	field string,
	values []string,
	maximum int,
	fail validationFailure,
) ([]string, int, error) {
	if len(values) > maximum {
		return nil, 0, fail(field, -1, ProblemTooMany)
	}
	result := slices.Clone(values)
	total := 0
	for index, value := range result {
		if err := validateValueWith(field, index, value, fail); err != nil {
			return nil, 0, err
		}
		total += len(value)
	}
	return result, total, nil
}

func copyEnvironment(values []string) ([]string, int, error) {
	return copyEnvironmentWith(values, specFailure)
}

func copyEnvironmentWith(values []string, fail validationFailure) ([]string, int, error) {
	result, total, err := copyValuesWith("environment", values, MaximumEnvironment, fail)
	if err != nil {
		return nil, 0, err
	}
	seen := make(map[string]struct{}, len(result))
	for index, value := range result {
		separator := strings.IndexByte(value, '=')
		if separator <= 0 {
			return nil, 0, fail("environment", index, ProblemMalformed)
		}
		name := strings.ToUpper(value[:separator])
		if _, duplicate := seen[name]; duplicate {
			return nil, 0, fail("environment", index, ProblemDuplicate)
		}
		seen[name] = struct{}{}
	}
	slices.Sort(result)
	return result, total, nil
}

func copyCapabilities(values []tool.Capability) ([]tool.Capability, int, error) {
	if len(values) > MaximumCapabilities {
		return nil, 0, specFailure("capabilities", -1, ProblemTooMany)
	}
	result := slices.Clone(values)
	seen := make(map[tool.Capability]struct{}, len(result))
	foundExecute := false
	total := 0
	for index, capability := range result {
		value := string(capability)
		if !validCapability(value) {
			return nil, 0, specFailure("capabilities", index, ProblemMalformed)
		}
		if _, duplicate := seen[capability]; duplicate {
			return nil, 0, specFailure("capabilities", index, ProblemDuplicate)
		}
		seen[capability] = struct{}{}
		foundExecute = foundExecute || capability == tool.CapabilityProcessExecute
		total += len(value)
	}
	if !foundExecute {
		return nil, 0, specFailure("capabilities", -1, ProblemMissingCapability)
	}
	slices.Sort(result)
	return result, total, nil
}

func validCapability(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	segmentStart := true
	for _, character := range value {
		switch {
		case character == '.':
			if segmentStart {
				return false
			}
			segmentStart = true
		case character >= 'a' && character <= 'z':
			segmentStart = false
		case !segmentStart && character >= '0' && character <= '9':
		default:
			return false
		}
	}
	return !segmentStart
}

func validateValueWith(field string, index int, value string, fail validationFailure) error {
	if !utf8.ValidString(value) {
		return fail(field, index, ProblemInvalidUTF8)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fail(field, index, ProblemContainsNUL)
	}
	if len(value) > MaximumValueBytes {
		return fail(field, index, ProblemTooLarge)
	}
	return nil
}

func specFailure(field string, index int, problem SpecProblem) error {
	return &SpecError{field: field, index: index, problem: problem}
}
