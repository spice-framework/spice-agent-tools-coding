package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// LookupError identifies a secret-safe executable-lookup input failure.
type LookupError struct {
	field   string
	index   int
	problem SpecProblem
}

func (failure *LookupError) Error() string {
	if failure == nil {
		return "invalid executable lookup"
	}
	if failure.index >= 0 {
		return fmt.Sprintf(
			"invalid executable lookup: %s[%d] is %s",
			failure.field,
			failure.index,
			failure.problem,
		)
	}
	return fmt.Sprintf("invalid executable lookup: %s is %s", failure.field, failure.problem)
}

func (failure *LookupError) Field() string        { return failure.field }
func (failure *LookupError) Index() int           { return failure.index }
func (failure *LookupError) Problem() SpecProblem { return failure.problem }
func (failure *LookupError) MarshalJSON() ([]byte, error) {
	return json.Marshal(failure.Error())
}

// Lookup is immutable executable-resolution intent. RequestedExecutable may be
// a natural platform name, a relative path interpreted from WorkingDirectory,
// or an absolute path. Environment is the exact explicit child environment;
// resolvers may use its platform search variables but never ambient process
// state. Lookup performs no filesystem or network access.
type Lookup struct {
	requestedExecutable string
	workingDirectory    string
	environment         []string
}

// NewLookup validates and defensively copies executable-resolution intent.
func NewLookup(
	requestedExecutable,
	workingDirectory string,
	environment []string,
) (Lookup, error) {
	if requestedExecutable == "" {
		return Lookup{}, lookupFailure("requested_executable", -1, ProblemRequired)
	}
	if err := validateValueWith(
		"requested_executable",
		-1,
		requestedExecutable,
		lookupFailure,
	); err != nil {
		return Lookup{}, err
	}
	if err := validatePathWith("working_directory", workingDirectory, lookupFailure); err != nil {
		return Lookup{}, err
	}
	copiedEnvironment, environmentBytes, err := copyEnvironmentWith(environment, lookupFailure)
	if err != nil {
		return Lookup{}, err
	}
	if len(requestedExecutable)+len(workingDirectory)+environmentBytes > MaximumSpecBytes {
		return Lookup{}, lookupFailure("lookup", -1, ProblemTooLarge)
	}
	return Lookup{
		requestedExecutable: requestedExecutable,
		workingDirectory:    workingDirectory,
		environment:         copiedEnvironment,
	}, nil
}

// Validate rejects a zero or corrupted lookup without performing I/O.
func (lookup Lookup) Validate() error {
	_, err := NewLookup(lookup.requestedExecutable, lookup.workingDirectory, lookup.environment)
	return err
}

func (lookup Lookup) RequestedExecutable() string { return lookup.requestedExecutable }
func (lookup Lookup) WorkingDirectory() string    { return lookup.workingDirectory }
func (lookup Lookup) Environment() []string       { return slices.Clone(lookup.environment) }

// Clone returns an independently backed immutable value.
func (lookup Lookup) Clone() Lookup {
	lookup.environment = slices.Clone(lookup.environment)
	return lookup
}

func (Lookup) String() string   { return "process.Lookup([REDACTED])" }
func (Lookup) GoString() string { return "process.Lookup([REDACTED])" }
func (Lookup) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "process.Lookup([REDACTED])")
}

func (Lookup) MarshalJSON() ([]byte, error) {
	return json.Marshal("process.Lookup([REDACTED])")
}

// ExecutableResolver resolves natural executable names and relative paths to
// the exact lexically canonical absolute executable required by Spec. Resolve's
// context bounds only the lookup. Implementations must use only Lookup's
// working directory and environment, perform no hidden network access, and
// return failures through NewFailure with OperationResolve. The caller must
// pass the returned path through NewSpec before launch.
type ExecutableResolver interface {
	Resolve(context.Context, Lookup) (string, error)
}

// ResolverFunc adapts an ordinary function for constructor injection.
type ResolverFunc func(context.Context, Lookup) (string, error)

func (resolver ResolverFunc) Resolve(ctx context.Context, lookup Lookup) (string, error) {
	return resolver(ctx, lookup)
}

func lookupFailure(field string, index int, problem SpecProblem) error {
	return &LookupError{field: field, index: index, problem: problem}
}
