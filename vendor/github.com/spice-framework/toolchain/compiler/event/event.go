// Package event compiles typed event topic markers and provider-owned
// listeners into deterministic application metadata. It never executes marker
// functions, providers, or listener methods.
package event

import (
	"fmt"
	"go/token"
	"go/types"
	"sort"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
	"github.com/spice-framework/toolchain/compiler/signature"
)

const (
	// TopicAnnotation identifies a generated typed event-topic marker.
	TopicAnnotation = "event.Topic"
	// ListenerAnnotation identifies a provider-owned event subscriber method.
	ListenerAnnotation = "event.Listener"

	eventPackagePath = "github.com/spice-framework/spice/event"
)

var errorType = types.Universe.Lookup("error").Type()

// Listener is one exact provider-owned event subscriber.
type Listener struct {
	Method           load.Symbol
	MethodID         string
	ProviderID       string
	Module           string
	Receiver         types.Type
	ReceiverTypeID   string
	Payload          types.Type
	PayloadTypeID    string
	Order            int
	Position         token.Position
	PhysicalPosition token.Position
}

// Topic is one generated event publisher and its deterministic subscribers.
type Topic struct {
	Marker           load.Symbol
	MarkerID         string
	Name             string
	ProviderID       string
	Module           string
	Publisher        types.Type
	PublisherTypeID  string
	Payload          types.Type
	PayloadTypeID    string
	Position         token.Position
	PhysicalPosition token.Position
	listeners        []Listener
}

// Listeners returns subscribers in runtime delivery order.
func (topic Topic) Listeners() []Listener {
	return append([]Listener(nil), topic.listeners...)
}

// Diagnostic is one deterministic source-positioned event contract failure.
type Diagnostic struct {
	Position         token.Position
	PhysicalPosition token.Position
	SymbolID         string
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

// Catalog contains immutable event metadata, synthetic publisher providers,
// and deterministic diagnostics.
type Catalog struct {
	topics      []Topic
	providers   []provider.Provider
	diagnostics []Diagnostic
}

// Topics returns topic metadata in stable marker identity order.
func (catalog Catalog) Topics() []Topic {
	result := make([]Topic, len(catalog.topics))
	copy(result, catalog.topics)
	for index := range result {
		result[index].listeners = append(
			[]Listener(nil),
			catalog.topics[index].listeners...,
		)
	}
	return result
}

// Providers returns synthetic exact event.Publisher[T] provider nodes.
func (catalog Catalog) Providers() []provider.Provider {
	result := make([]provider.Provider, len(catalog.providers))
	copy(result, catalog.providers)
	for index := range result {
		result[index].Dependencies = append(
			[]provider.Dependency(nil),
			catalog.providers[index].Dependencies...,
		)
	}
	return result
}

// Diagnostics returns a defensive copy in stable source order.
func (catalog Catalog) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), catalog.diagnostics...)
}

// Build compiles event markers and listeners from one typed program and exact
// provider catalog.
func Build(
	program *load.Program,
	resolution resolve.Result,
	providers provider.Catalog,
	modules modulith.Model,
) Catalog {
	catalog := Catalog{}
	if program == nil {
		catalog.diagnostics = []Diagnostic{{
			Kind:    "internal",
			Message: "event catalog requires a loaded program",
		}}
		return catalog
	}
	symbols := symbolIndex(program)
	listeners, listenerDiagnostics := buildListeners(
		program,
		resolution,
		symbols,
		providers.Providers(),
		modules,
	)
	catalog.diagnostics = append(
		catalog.diagnostics,
		listenerDiagnostics...,
	)
	topics, topicProviders, usedListeners, topicDiagnostics := buildTopics(
		program,
		resolution,
		symbols,
		listeners,
		modules,
	)
	catalog.topics = topics
	catalog.providers = topicProviders
	catalog.diagnostics = append(
		catalog.diagnostics,
		topicDiagnostics...,
	)
	for _, listener := range listeners {
		if usedListeners[listener.MethodID] {
			continue
		}
		catalog.diagnostics = append(catalog.diagnostics, Diagnostic{
			Position:         listener.Position,
			PhysicalPosition: listener.PhysicalPosition,
			SymbolID:         listener.MethodID,
			Kind:             "unassigned-listener",
			Message: fmt.Sprintf(
				"@%s method %q is not selected by any @%s marker for payload %s",
				ListenerAnnotation,
				listener.Method.DisplayLabel,
				TopicAnnotation,
				listener.PayloadTypeID,
			),
		})
	}
	sort.SliceStable(catalog.topics, func(left, right int) bool {
		return catalog.topics[left].MarkerID <
			catalog.topics[right].MarkerID
	})
	sort.SliceStable(catalog.providers, func(left, right int) bool {
		return catalog.providers[left].SymbolID <
			catalog.providers[right].SymbolID
	})
	sortDiagnostics(catalog.diagnostics)
	return catalog
}

func buildListeners(
	program *load.Program,
	resolution resolve.Result,
	symbols map[string]load.Symbol,
	providers []provider.Provider,
	modules modulith.Model,
) ([]Listener, []Diagnostic) {
	contextType := signature.ContextType(program)
	occurrences := annotationOccurrences(
		resolution,
		sdk.ContributionEventListener,
	)
	var listeners []Listener
	var diagnostics []Diagnostic
	for _, symbolID := range sortedOccurrenceIDs(occurrences) {
		values := occurrences[symbolID]
		if len(values) == 0 {
			continue
		}
		occurrence := values[0]
		if len(values) != 1 {
			diagnostics = append(
				diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-listener",
					fmt.Sprintf(
						"method %q carries @%s more than once",
						occurrence.Name,
						ListenerAnnotation,
					),
				),
			)
			continue
		}
		method, found := symbols[symbolID]
		if !found {
			diagnostics = append(
				diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"missing-listener-symbol",
					fmt.Sprintf(
						"@%s target %q has no stable typed method symbol",
						ListenerAnnotation,
						occurrence.Name,
					),
				),
			)
			continue
		}
		listener, problem := analyzeListener(
			occurrence,
			method,
			contextType,
			providers,
			modules,
		)
		if problem != nil {
			diagnostics = append(diagnostics, *problem)
			continue
		}
		listeners = append(listeners, listener)
	}
	sort.SliceStable(listeners, func(left, right int) bool {
		return listeners[left].MethodID < listeners[right].MethodID
	})
	return listeners, diagnostics
}

func analyzeListener(
	occurrence resolve.Occurrence,
	method load.Symbol,
	contextType types.Type,
	providers []provider.Provider,
	modules modulith.Model,
) (Listener, *Diagnostic) {
	signatureValue := method.Signature
	if occurrence.Target != annotation.TargetMethod ||
		method.Kind != load.SymbolMethod ||
		signatureValue == nil ||
		signatureValue.Recv() == nil {
		return listenerFailure(
			occurrence,
			method.ID,
			"invalid-listener-target",
			"event listeners must target ordinary methods",
		)
	}
	if !token.IsExported(method.Name) ||
		signatureValue.Variadic() ||
		hasTypeParameters(signatureValue) {
		return listenerFailure(
			occurrence,
			method.ID,
			"invalid-listener-method",
			"event listener methods must be exported, non-generic, and non-variadic",
		)
	}
	if contextType == nil ||
		signatureValue.Params().Len() != 2 ||
		!types.Identical(
			signatureValue.Params().At(0).Type(),
			contextType,
		) ||
		signatureValue.Results().Len() != 1 ||
		!types.Identical(
			signatureValue.Results().At(0).Type(),
			errorType,
		) {
		return listenerFailure(
			occurrence,
			method.ID,
			"listener-signature",
			"event listener methods require exact signature func(context.Context, Event) error",
		)
	}
	receiver := signatureValue.Recv().Type()
	owner, found := exactProvider(receiver, providers)
	if !found {
		return listenerFailure(
			occurrence,
			method.ID,
			"listener-provider",
			fmt.Sprintf(
				"event listener receiver %s requires exactly one exact @Bean provider",
				provider.TypeID(receiver),
			),
		)
	}
	order, orderProblem := listenerOrder(occurrence)
	if orderProblem != "" {
		return listenerFailure(
			occurrence,
			method.ID,
			"listener-arguments",
			orderProblem,
		)
	}
	payload := signatureValue.Params().At(1).Type()
	return Listener{
		Method:           method,
		MethodID:         method.ID,
		ProviderID:       owner.SymbolID,
		Module:           moduleID(modules, method.PackagePath),
		Receiver:         receiver,
		ReceiverTypeID:   provider.TypeID(receiver),
		Payload:          payload,
		PayloadTypeID:    provider.TypeID(payload),
		Order:            order,
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
	}, nil
}

func buildTopics(
	program *load.Program,
	resolution resolve.Result,
	symbols map[string]load.Symbol,
	listeners []Listener,
	modules modulith.Model,
) ([]Topic, []provider.Provider, map[string]bool, []Diagnostic) {
	occurrences := annotationOccurrences(
		resolution,
		sdk.ContributionEventTopic,
	)
	fileSets := packageFileSets(program)
	var topics []Topic
	var providers []provider.Provider
	used := make(map[string]bool)
	var diagnostics []Diagnostic
	for _, symbolID := range sortedOccurrenceIDs(occurrences) {
		values := occurrences[symbolID]
		if len(values) == 0 {
			continue
		}
		occurrence := values[0]
		if len(values) != 1 {
			diagnostics = append(
				diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"duplicate-topic",
					fmt.Sprintf(
						"function %q carries @%s more than once",
						occurrence.Name,
						TopicAnnotation,
					),
				),
			)
			continue
		}
		marker, found := symbols[symbolID]
		if !found {
			diagnostics = append(
				diagnostics,
				occurrenceDiagnostic(
					occurrence,
					"missing-topic-symbol",
					fmt.Sprintf(
						"@%s target %q has no stable typed function symbol",
						TopicAnnotation,
						occurrence.Name,
					),
				),
			)
			continue
		}
		topic, synthetic, selected, problem := analyzeTopic(
			occurrence,
			marker,
			listeners,
			modules,
			fileSets[marker.PackagePath],
		)
		if problem != nil {
			diagnostics = append(diagnostics, *problem)
			continue
		}
		for _, listener := range selected {
			if used[listener.MethodID] {
				diagnostics = append(
					diagnostics,
					occurrenceDiagnostic(
						occurrence,
						"duplicate-listener-selection",
						fmt.Sprintf(
							"event listener %q is selected by more than one topic marker",
							listener.Method.DisplayLabel,
						),
					),
				)
				continue
			}
			used[listener.MethodID] = true
		}
		topics = append(topics, topic)
		providers = append(providers, synthetic)
	}
	return topics, providers, used, diagnostics
}

func analyzeTopic(
	occurrence resolve.Occurrence,
	marker load.Symbol,
	listeners []Listener,
	modules modulith.Model,
	fileSet *token.FileSet,
) (Topic, provider.Provider, []Listener, *Diagnostic) {
	publisher, payload, problem := validateTopicMarker(
		occurrence,
		marker,
	)
	if problem != nil {
		return Topic{}, provider.Provider{}, nil, problem
	}
	selected, dependencies, problem := selectTopicListeners(
		occurrence,
		marker,
		payload,
		listeners,
		fileSet,
	)
	if problem != nil {
		return Topic{}, provider.Provider{}, nil, problem
	}
	sort.SliceStable(selected, func(left, right int) bool {
		if selected[left].Order != selected[right].Order {
			return selected[left].Order < selected[right].Order
		}
		if selected[left].Module != selected[right].Module {
			return selected[left].Module < selected[right].Module
		}
		return selected[left].MethodID < selected[right].MethodID
	})
	topic := Topic{
		Marker:           marker,
		MarkerID:         marker.ID,
		Name:             marker.Name,
		ProviderID:       marker.ID,
		Module:           moduleID(modules, marker.PackagePath),
		Publisher:        publisher,
		PublisherTypeID:  provider.TypeID(publisher),
		Payload:          payload,
		PayloadTypeID:    provider.TypeID(payload),
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
		listeners:        selected,
	}
	synthetic := provider.Provider{
		Source:           provider.SourceEvent,
		Symbol:           marker,
		SymbolID:         marker.ID,
		Name:             marker.Name,
		PackagePath:      marker.PackagePath,
		Position:         marker.Position,
		PhysicalPosition: marker.PhysicalPosition,
		Output:           publisher,
		OutputTypeID:     provider.TypeID(publisher),
		Dependencies:     dependencies,
		ReturnsError:     true,
	}
	return topic, synthetic, selected, nil
}

func validateTopicMarker(
	occurrence resolve.Occurrence,
	marker load.Symbol,
) (types.Type, types.Type, *Diagnostic) {
	signatureValue := marker.Signature
	if occurrence.Target != annotation.TargetFunction ||
		marker.Kind != load.SymbolFunction ||
		signatureValue == nil ||
		signatureValue.Recv() != nil {
		return nil, nil, topicProblem(
			occurrence,
			marker.ID,
			"invalid-topic-target",
			"event topics must target ordinary package-level functions",
		)
	}
	if !token.IsExported(marker.Name) ||
		signatureValue.Variadic() ||
		hasTypeParameters(signatureValue) {
		return nil, nil, topicProblem(
			occurrence,
			marker.ID,
			"invalid-topic-marker",
			"event topic markers must be exported, non-generic, and non-variadic",
		)
	}
	if len(occurrence.Annotation.Arguments) != 0 {
		return nil, nil, topicProblem(
			occurrence,
			marker.ID,
			"topic-arguments",
			"event topic markers do not accept arguments",
		)
	}
	if signatureValue.Results().Len() != 1 {
		return nil, nil, topicProblem(
			occurrence,
			marker.ID,
			"topic-result",
			"event topic markers require exactly one event.Publisher[Event] result",
		)
	}
	publisher := signatureValue.Results().At(0).Type()
	payload, validPublisher := publisherPayload(publisher)
	if !validPublisher || !exportedNamedValue(payload) {
		return nil, nil, topicProblem(
			occurrence,
			marker.ID,
			"topic-result",
			"event topic markers must return exact event.Publisher[Event] for an exported named event value",
		)
	}
	return publisher, payload, nil
}

func selectTopicListeners(
	occurrence resolve.Occurrence,
	marker load.Symbol,
	payload types.Type,
	listeners []Listener,
	fileSet *token.FileSet,
) ([]Listener, []provider.Dependency, *Diagnostic) {
	signatureValue := marker.Signature
	if signatureValue == nil {
		return nil, nil, topicProblem(
			occurrence,
			marker.ID,
			"invalid-topic-target",
			"event topic marker has no typed signature",
		)
	}
	selected := make([]Listener, 0, signatureValue.Params().Len())
	dependencies := make(
		[]provider.Dependency,
		signatureValue.Params().Len(),
	)
	selectedMethods := make(map[string]struct{})
	for index := range signatureValue.Params().Len() {
		parameter := signatureValue.Params().At(index)
		matches := listenerMatches(parameter.Type(), payload, listeners)
		if len(matches) != 1 {
			return nil, nil, topicProblem(
				occurrence,
				marker.ID,
				"topic-listener",
				fmt.Sprintf(
					"event topic marker parameter %d type %s requires exactly one @%s method for payload %s, found %d",
					index,
					provider.TypeID(parameter.Type()),
					ListenerAnnotation,
					provider.TypeID(payload),
					len(matches),
				),
			)
		}
		if _, duplicate := selectedMethods[matches[0].MethodID]; duplicate {
			return nil, nil, topicProblem(
				occurrence,
				marker.ID,
				"duplicate-topic-listener",
				fmt.Sprintf(
					"event topic marker selects listener %q more than once",
					matches[0].Method.DisplayLabel,
				),
			)
		}
		selectedMethods[matches[0].MethodID] = struct{}{}
		selected = append(selected, matches[0])
		dependencies[index] = topicDependency(
			parameter,
			index,
			marker,
			fileSet,
		)
	}
	return selected, dependencies, nil
}

func publisherPayload(value types.Type) (types.Type, bool) {
	named, ok := types.Unalias(value).(*types.Named)
	if !ok ||
		named.Obj() == nil ||
		named.Obj().Pkg() == nil ||
		named.Obj().Pkg().Path() != eventPackagePath ||
		named.Obj().Name() != "Publisher" ||
		named.TypeArgs() == nil ||
		named.TypeArgs().Len() != 1 {
		return nil, false
	}
	return named.TypeArgs().At(0), true
}

func exportedNamedValue(value types.Type) bool {
	named, ok := types.Unalias(value).(*types.Named)
	return ok &&
		named.Obj() != nil &&
		token.IsExported(named.Obj().Name())
}

func listenerMatches(
	receiver types.Type,
	payload types.Type,
	listeners []Listener,
) []Listener {
	var result []Listener
	for _, listener := range listeners {
		if types.Identical(receiver, listener.Receiver) &&
			types.Identical(payload, listener.Payload) {
			result = append(result, listener)
		}
	}
	return result
}

func exactProvider(
	value types.Type,
	providers []provider.Provider,
) (provider.Provider, bool) {
	var matches []provider.Provider
	for _, item := range providers {
		if types.Identical(value, item.Output) {
			matches = append(matches, item)
		}
	}
	if len(matches) != 1 {
		return provider.Provider{}, false
	}
	return matches[0], true
}

func listenerOrder(occurrence resolve.Occurrence) (int, string) {
	if contribution, found := occurrence.DescriptorContribution(
		sdk.ContributionEventListener,
	); found {
		return int(contribution.EventListener.Order), ""
	}
	order := 0
	seen := false
	for _, argument := range occurrence.Annotation.Arguments {
		if argument.Name != "order" {
			return 0, fmt.Sprintf(
				"@%s does not define argument %q",
				ListenerAnnotation,
				argument.Name,
			)
		}
		if seen {
			return 0, fmt.Sprintf(
				"@%s assigns argument %q more than once",
				ListenerAnnotation,
				argument.Name,
			)
		}
		if argument.Value.Kind != annotation.KindInteger {
			return 0, fmt.Sprintf(
				"@%s argument %q requires integer",
				ListenerAnnotation,
				argument.Name,
			)
		}
		seen = true
		order = int(argument.Value.Integer)
	}
	return order, ""
}

func topicDependency(
	parameter *types.Var,
	index int,
	marker load.Symbol,
	fileSet *token.FileSet,
) provider.Dependency {
	name := parameter.Name()
	if name == "" {
		name = fmt.Sprintf("listener%d", index)
	}
	position := marker.Position
	physical := marker.PhysicalPosition
	if fileSet != nil && parameter.Pos().IsValid() {
		position = fileSet.PositionFor(parameter.Pos(), true)
		physical = fileSet.PositionFor(parameter.Pos(), false)
	}
	return provider.Dependency{
		Index:            index,
		Name:             name,
		Type:             parameter.Type(),
		TypeID:           provider.TypeID(parameter.Type()),
		Position:         position,
		PhysicalPosition: physical,
	}
}

func annotationOccurrences(
	resolution resolve.Result,
	kind sdk.ContributionKind,
) map[string][]resolve.Occurrence {
	result := make(map[string][]resolve.Occurrence)
	for _, occurrence := range resolution.Occurrences {
		if occurrence.HasContribution(kind) {
			result[occurrence.SymbolID] = append(
				result[occurrence.SymbolID],
				occurrence,
			)
		}
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

func sortedOccurrenceIDs(
	occurrences map[string][]resolve.Occurrence,
) []string {
	result := make([]string, 0, len(occurrences))
	for symbolID := range occurrences {
		result = append(result, symbolID)
	}
	sort.Strings(result)
	return result
}

func symbolIndex(program *load.Program) map[string]load.Symbol {
	result := make(map[string]load.Symbol)
	for _, symbol := range program.Symbols() {
		result[symbol.ID] = symbol
	}
	return result
}

func packageFileSets(program *load.Program) map[string]*token.FileSet {
	result := make(map[string]*token.FileSet)
	for _, packageValue := range program.Packages() {
		if packageValue.Raw != nil {
			result[packageValue.Path] = packageValue.Raw.Fset
		}
	}
	return result
}

func moduleID(modules modulith.Model, packagePath string) string {
	if owner, found := modules.Owner(packagePath); found {
		return owner.ID
	}
	return packagePath
}

func hasTypeParameters(value *types.Signature) bool {
	receiverParameters := value.RecvTypeParams()
	methodParameters := value.TypeParams()
	return (receiverParameters != nil && receiverParameters.Len() != 0) ||
		(methodParameters != nil && methodParameters.Len() != 0)
}

func listenerFailure(
	occurrence resolve.Occurrence,
	symbolID string,
	kind string,
	message string,
) (Listener, *Diagnostic) {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	diagnostic.SymbolID = symbolID
	return Listener{}, &diagnostic
}

func topicProblem(
	occurrence resolve.Occurrence,
	symbolID string,
	kind string,
	message string,
) *Diagnostic {
	diagnostic := occurrenceDiagnostic(occurrence, kind, message)
	diagnostic.SymbolID = symbolID
	return &diagnostic
}

func occurrenceDiagnostic(
	occurrence resolve.Occurrence,
	kind string,
	message string,
) Diagnostic {
	return Diagnostic{
		Position:         occurrence.DisplayPosition,
		PhysicalPosition: physicalPosition(occurrence),
		SymbolID:         occurrence.SymbolID,
		Kind:             kind,
		Message:          message,
	}
}

func physicalPosition(occurrence resolve.Occurrence) token.Position {
	return token.Position{
		Filename: occurrence.PhysicalFile,
		Offset:   occurrence.PhysicalOffset,
	}
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
		if leftDiagnostic.SymbolID != rightDiagnostic.SymbolID {
			return leftDiagnostic.SymbolID < rightDiagnostic.SymbolID
		}
		return leftDiagnostic.Message < rightDiagnostic.Message
	})
}
