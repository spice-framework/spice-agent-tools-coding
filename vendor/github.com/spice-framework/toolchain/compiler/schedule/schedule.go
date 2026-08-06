// Package schedule compiles provider-owned fixed-delay annotations into
// deterministic metadata. It validates source only and never invokes methods.
package schedule

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"time"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
	"github.com/spice-framework/toolchain/compiler/signature"
)

const (
	// Annotation identifies the built-in fixed-delay scheduling annotation.
	Annotation = "schedule.FixedDelay"

	acceptedMethodSignature = "func(receiver)(context.Context) error"
)

var errorType = types.Universe.Lookup("error").Type()

// Job is one immutable, provider-owned scheduled method.
type Job struct {
	Method           load.Symbol
	MethodID         string
	ProviderID       string
	Receiver         types.Type
	ReceiverTypeID   string
	Position         token.Position
	PhysicalPosition token.Position
	InitialDelay     time.Duration
	Delay            time.Duration
	ContinueOnError  bool
}

// Diagnostic is one source-positioned scheduling contract failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	MethodID         string
	ProviderID       string
	Kind             string
	Message          string
}

// Error renders a compiler-style diagnostic.
func (diagnostic Diagnostic) Error() string {
	position := diagnostic.Position
	if position.Filename == "" {
		position.Filename = "<unknown>"
	}
	if position.Line <= 0 {
		position.Line = 1
	}
	if position.Column <= 0 {
		position.Column = 1
	}
	return fmt.Sprintf(
		"%s:%d:%d: %s",
		position.Filename,
		position.Line,
		position.Column,
		diagnostic.Message,
	)
}

// Catalog is immutable scheduling metadata and deterministic diagnostics.
type Catalog struct {
	jobs        []Job
	diagnostics []Diagnostic
}

// Jobs returns scheduled jobs in stable method identity order.
func (catalog Catalog) Jobs() []Job {
	return append([]Job(nil), catalog.jobs...)
}

// Diagnostics returns a defensive copy in stable source order.
func (catalog Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), catalog.diagnostics...)
}

// Build validates every @schedule.FixedDelay occurrence against the existing
// typed program and exact provider catalog.
func Build(
	program *load.Program,
	resolution resolve.Result,
	providers provider.Catalog,
) Catalog {
	catalog := Catalog{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{
			Kind:    "internal",
			Message: "schedule catalog requires a loaded program",
		}}
		return catalog
	}
	contextType := signature.ContextType(program)
	symbols := make(map[string]load.Symbol)
	for _, symbol := range program.Symbols() {
		symbols[symbol.ID] = symbol
	}
	occurrences := scheduledOccurrences(resolution)
	for _, methodID := range sortedMethodIDs(occurrences) {
		methodOccurrences := occurrences[methodID]
		if len(methodOccurrences) == 0 {
			continue
		}
		occurrence := methodOccurrences[0]
		if len(methodOccurrences) != 1 {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-annotation",
					fmt.Sprintf(
						"method %q carries @%s more than once; scheduling annotations are not repeatable",
						occurrence.Name,
						Annotation,
					),
				),
			)
			continue
		}
		symbol, found := symbols[methodID]
		if !found {
			catalog.diagnostics = append(
				catalog.diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"missing-symbol",
					fmt.Sprintf(
						"@%s target %q has no stable typed method symbol",
						Annotation,
						occurrence.Name,
					),
				),
			)
			continue
		}
		job, diagnostic := analyzeJob(
			occurrence,
			symbol,
			contextType,
			providers.Providers(),
		)
		if diagnostic != nil {
			catalog.diagnostics = append(catalog.diagnostics, *diagnostic)
			continue
		}
		catalog.jobs = append(catalog.jobs, job)
	}
	sort.SliceStable(catalog.jobs, func(left, right int) bool {
		return catalog.jobs[left].MethodID < catalog.jobs[right].MethodID
	})
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func scheduledOccurrences(
	resolution resolve.Result,
) map[string][]resolve.Occurrence {
	result := make(map[string][]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		if !occurrence.HasContribution(sdk.ContributionSchedule) {
			continue
		}
		result[occurrence.SymbolID] = append(
			result[occurrence.SymbolID],
			occurrence,
		)
	}
	for _, occurrences := range result {
		sort.SliceStable(occurrences, func(left, right int) bool {
			if occurrences[left].PhysicalFile !=
				occurrences[right].PhysicalFile {
				return occurrences[left].PhysicalFile <
					occurrences[right].PhysicalFile
			}
			return occurrences[left].PhysicalOffset <
				occurrences[right].PhysicalOffset
		})
	}
	return result
}

func sortedMethodIDs(
	occurrences map[string][]resolve.Occurrence,
) []string {
	result := make([]string, 0, len(occurrences))
	for methodID := range occurrences {
		result = append(result, methodID)
	}
	sort.Strings(result)
	return result
}

func analyzeJob(
	occurrence resolve.Occurrence,
	method load.Symbol,
	contextType types.Type,
	providers []provider.Provider,
) (Job, *Diagnostic) {
	receiver, problem := validateMethod(occurrence, method, contextType)
	if problem != nil {
		return jobFailure(occurrence, method, problem)
	}
	owner, problem := providerOwner(receiver, providers)
	if problem != nil {
		return jobFailure(occurrence, method, problem)
	}
	options, problem := parseOptions(occurrence)
	if problem != nil {
		return jobFailure(occurrence, method, problem)
	}
	return Job{
		Method:         method,
		MethodID:       method.ID,
		ProviderID:     owner.SymbolID,
		Receiver:       receiver,
		ReceiverTypeID: provider.TypeID(receiver),
		Position:       occurrence.DisplayPosition,
		PhysicalPosition: token.Position{
			Filename: occurrence.PhysicalFile,
			Offset:   occurrence.PhysicalOffset,
		},
		InitialDelay:    options.initialDelay,
		Delay:           options.delay,
		ContinueOnError: options.continueOnError,
	}, nil
}

type problem struct {
	kind   string
	reason string
}

func validateMethod(
	occurrence resolve.Occurrence,
	method load.Symbol,
	contextType types.Type,
) (types.Type, *problem) {
	if occurrence.Target != annotation.TargetMethod ||
		method.Kind != load.SymbolMethod ||
		method.Signature == nil ||
		method.Signature.Recv() == nil {
		return nil, &problem{
			kind:   "invalid-target",
			reason: "fixed-delay jobs must target ordinary methods",
		}
	}
	methodSignature := method.Signature
	if hasTypeParameters(methodSignature) {
		return nil, &problem{
			kind:   "generic-receiver",
			reason: "receiver-generic scheduled methods are not supported",
		}
	}
	if methodSignature.Variadic() {
		return nil, &problem{
			kind:   "variadic",
			reason: "scheduled methods must be non-variadic",
		}
	}
	if methodSignature.Params().Len() != 1 {
		return nil, &problem{
			kind: "parameter-count",
			reason: fmt.Sprintf(
				"scheduled methods require exactly one explicit context parameter, got %d",
				methodSignature.Params().Len(),
			),
		}
	}
	if contextType == nil {
		return nil, &problem{
			kind: "context-type",
			reason: "canonical context.Context identity could not be " +
				"established safely from the loaded Go 1.26 type universe",
		}
	}
	if !types.Identical(
		methodSignature.Params().At(0).Type(),
		contextType,
	) {
		return nil, &problem{
			kind: "context-type",
			reason: fmt.Sprintf(
				"parameter 0 must be the exact loaded context.Context type, got %s",
				provider.TypeID(methodSignature.Params().At(0).Type()),
			),
		}
	}
	if methodSignature.Results().Len() != 1 {
		return nil, &problem{
			kind: "result-count",
			reason: fmt.Sprintf(
				"scheduled methods require exactly one error result, got %d",
				methodSignature.Results().Len(),
			),
		}
	}
	if !types.Identical(
		methodSignature.Results().At(0).Type(),
		errorType,
	) {
		return nil, &problem{
			kind: "result-type",
			reason: fmt.Sprintf(
				"result 0 must be the exact predeclared error type, got %s",
				provider.TypeID(methodSignature.Results().At(0).Type()),
			),
		}
	}
	return methodSignature.Recv().Type(), nil
}

func hasTypeParameters(methodSignature *types.Signature) bool {
	receiverParameters := methodSignature.RecvTypeParams()
	methodParameters := methodSignature.TypeParams()
	return (receiverParameters != nil && receiverParameters.Len() > 0) ||
		(methodParameters != nil && methodParameters.Len() > 0)
}

func providerOwner(
	receiver types.Type,
	providers []provider.Provider,
) (provider.Provider, *problem) {
	var matches []provider.Provider
	for _, candidate := range providers {
		if types.Identical(receiver, candidate.Output) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return provider.Provider{}, &problem{
			kind: "receiver-provider",
			reason: fmt.Sprintf(
				"no @Bean provider produces exact receiver type %s; pointer/value convenience, assignability, interface implementation, and method promotion do not establish scheduling ownership",
				provider.TypeID(receiver),
			),
		}
	}
	if len(matches) != 1 {
		return provider.Provider{}, &problem{
			kind: "ambiguous-provider",
			reason: fmt.Sprintf(
				"receiver type %s is produced by %d providers; scheduling ownership requires exactly one",
				provider.TypeID(receiver),
				len(matches),
			),
		}
	}
	return matches[0], nil
}

type options struct {
	initialDelay    time.Duration
	delay           time.Duration
	continueOnError bool
}

func parseOptions(occurrence resolve.Occurrence) (options, *problem) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionSchedule,
	); found {
		delay, delayProblem := durationValue(
			"delay",
			contribution.Schedule.Delay,
			true,
		)
		if delayProblem != nil {
			return options{}, delayProblem
		}
		initialDelay, initialProblem := durationValue(
			"initialDelay",
			contribution.Schedule.InitialDelay,
			false,
		)
		if initialProblem != nil {
			return options{}, initialProblem
		}
		return options{
			delay:           delay,
			initialDelay:    initialDelay,
			continueOnError: contribution.Schedule.ContinueOnError,
		}, nil
	}
	result := options{}
	seen := make(map[string]struct{})
	delayFound := false
	for _, argument := range occurrence.Annotation.Arguments {
		if argument.Name == "" {
			return options{}, &problem{
				kind:   "arguments",
				reason: "fixed-delay scheduling accepts only named arguments",
			}
		}
		if _, duplicate := seen[argument.Name]; duplicate {
			return options{}, &problem{
				kind: "arguments",
				reason: fmt.Sprintf(
					"argument %q is assigned more than once",
					argument.Name,
				),
			}
		}
		seen[argument.Name] = struct{}{}
		switch argument.Name {
		case "delay":
			delay, durationProblem := durationOption(
				argument,
				true,
			)
			if durationProblem != nil {
				return options{}, durationProblem
			}
			result.delay = delay
			delayFound = true
		case "initialDelay":
			initialDelay, durationProblem := durationOption(
				argument,
				false,
			)
			if durationProblem != nil {
				return options{}, durationProblem
			}
			result.initialDelay = initialDelay
		case "continueOnError":
			if argument.Value.Kind != annotation.KindBoolean {
				return options{}, &problem{
					kind: "arguments",
					reason: fmt.Sprintf(
						"argument %q requires boolean",
						argument.Name,
					),
				}
			}
			result.continueOnError = argument.Value.Boolean
		default:
			return options{}, &problem{
				kind: "arguments",
				reason: fmt.Sprintf(
					"fixed-delay scheduling does not define argument %q",
					argument.Name,
				),
			}
		}
	}
	if !delayFound {
		return options{}, &problem{
			kind:   "arguments",
			reason: `required argument "delay" is missing`,
		}
	}
	return result, nil
}

func durationOption(
	argument annotation.Argument,
	positive bool,
) (time.Duration, *problem) {
	if argument.Value.Kind != annotation.KindString {
		return 0, &problem{
			kind: "arguments",
			reason: fmt.Sprintf(
				"argument %q requires a duration string",
				argument.Name,
			),
		}
	}
	raw := argument.Value.String
	return durationValue(argument.Name, raw, positive)
}

func durationValue(
	name string,
	raw string,
	positive bool,
) (time.Duration, *problem) {
	if raw == "" && !positive {
		return 0, nil
	}
	if raw == "" || strings.TrimSpace(raw) != raw {
		return 0, &problem{
			kind: "duration",
			reason: fmt.Sprintf(
				"argument %q must be a non-empty duration without surrounding whitespace",
				name,
			),
		}
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, &problem{
			kind: "duration",
			reason: fmt.Sprintf(
				"argument %q is not a valid Go duration: %v",
				name,
				err,
			),
		}
	}
	if positive && value <= 0 {
		return 0, &problem{
			kind: "duration",
			reason: fmt.Sprintf(
				"argument %q must be positive",
				name,
			),
		}
	}
	if !positive && value < 0 {
		return 0, &problem{
			kind: "duration",
			reason: fmt.Sprintf(
				"argument %q must not be negative",
				name,
			),
		}
	}
	return value, nil
}

func jobFailure(
	occurrence resolve.Occurrence,
	method load.Symbol,
	jobProblem *problem,
) (Job, *Diagnostic) {
	diagnostic := symbolDiagnostic(
		occurrence,
		method,
		jobProblem.kind,
		fmt.Sprintf(
			"@%s method %s is invalid: %s; accepted form is %s",
			Annotation,
			methodLabel(method),
			jobProblem.reason,
			acceptedMethodSignature,
		),
	)
	return Job{}, &diagnostic
}

func methodLabel(method load.Symbol) string {
	if method.DisplayLabel != "" {
		return method.DisplayLabel
	}
	if method.ID != "" {
		return method.ID
	}
	return method.Name
}

func occurrenceDiagnostic(
	occurrence resolve.Occurrence,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position: occurrence.DisplayPosition,
		PhysicalPosition: token.Position{
			Filename: occurrence.PhysicalFile,
			Offset:   occurrence.PhysicalOffset,
		},
		MethodID: occurrence.SymbolID,
		Kind:     kind,
		Message:  message,
	}
}

func symbolDiagnostic(
	occurrence resolve.Occurrence,
	method load.Symbol,
	kind string,
	message string,
) Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	if diagnostic.Position.Filename == "" {
		diagnostic.Position = method.Position
	}
	if diagnostic.PhysicalPosition.Filename == "" {
		diagnostic.PhysicalPosition = method.PhysicalPosition
	}
	return diagnostic
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(left, right int) bool {
		leftDiagnostic := diagnostics[left]
		rightDiagnostic := diagnostics[right]
		if leftDiagnostic.PhysicalPosition.Filename !=
			rightDiagnostic.PhysicalPosition.Filename {
			return leftDiagnostic.PhysicalPosition.Filename <
				rightDiagnostic.PhysicalPosition.Filename
		}
		if leftDiagnostic.PhysicalPosition.Offset !=
			rightDiagnostic.PhysicalPosition.Offset {
			return leftDiagnostic.PhysicalPosition.Offset <
				rightDiagnostic.PhysicalPosition.Offset
		}
		if leftDiagnostic.Kind != rightDiagnostic.Kind {
			return leftDiagnostic.Kind < rightDiagnostic.Kind
		}
		if leftDiagnostic.MethodID != rightDiagnostic.MethodID {
			return leftDiagnostic.MethodID < rightDiagnostic.MethodID
		}
		return leftDiagnostic.Message < rightDiagnostic.Message
	})
}
