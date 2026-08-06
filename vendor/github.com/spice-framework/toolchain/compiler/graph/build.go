package graph

import (
	"container/heap"
	"fmt"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/provider"
)

// Build validates exact provider dependencies from one already validated
// catalog. It never reloads source, inspects function bodies, or executes a
// provider. A catalog that already has diagnostics is not graphed.
func Build(catalog provider.Catalog) Result {
	if len(catalog.Diagnostics()) != 0 {
		return Result{}
	}

	result := Result{providers: catalog.Providers()}
	result.nodes = make([]Node, len(result.providers))
	for index := range result.providers {
		result.nodes[index] = Node{ProviderID: result.providers[index].SymbolID, provider: &result.providers[index]}
	}

	index := buildTypeIndex(result.providers)
	adjacency := make([][]int, len(result.providers))
	connectDependencies(&result, index, adjacency)

	sort.SliceStable(result.edges, func(i, j int) bool {
		left, right := result.edges[i], result.edges[j]
		if left.ConsumerID != right.ConsumerID {
			return left.ConsumerID < right.ConsumerID
		}
		if left.ParameterIndex != right.ParameterIndex {
			return left.ParameterIndex < right.ParameterIndex
		}
		if left.CollectionIndex != right.CollectionIndex {
			return left.CollectionIndex < right.CollectionIndex
		}
		return left.DependencyID < right.DependencyID
	})
	for index := range adjacency {
		adjacency[index] = sortedUnique(adjacency[index], result.providers)
	}

	for _, component := range stronglyConnected(adjacency, result.providers) {
		if len(component) > 1 || containsIndex(adjacency[component[0]], component[0]) {
			result.diagnostics = append(result.diagnostics, cycleDiagnostic(component, adjacency, result.providers))
		}
	}
	sortDiagnostics(result.diagnostics)
	if len(result.diagnostics) == 0 {
		result.order = constructionOrder(adjacency, result.providers)
	}
	return result
}

func connectDependencies(
	result *Result,
	index typeIndex,
	adjacency [][]int,
) {
	for consumerIndex := range result.providers {
		consumer := &result.providers[consumerIndex]
		for _, input := range consumer.Dependencies {
			connectDependency(
				result,
				index,
				adjacency,
				consumerIndex,
				input,
			)
		}
	}
}

func connectDependency(
	result *Result,
	index typeIndex,
	adjacency [][]int,
	consumerIndex int,
	input provider.Dependency,
) {
	kind, candidates := dependencyCandidates(
		index,
		input,
		result.providers,
	)
	if kind == provider.DependencySlice ||
		kind == provider.DependencyMap {
		connectCollection(
			result,
			adjacency,
			consumerIndex,
			input,
			kind,
			candidates,
		)
		return
	}
	connectSingle(
		result,
		adjacency,
		consumerIndex,
		input,
		kind,
		candidates,
	)
}

func dependencyCandidates(
	index typeIndex,
	input provider.Dependency,
	providers []provider.Provider,
) (provider.DependencyKind, []int) {
	kind := input.Kind
	var candidates []int
	if kind == provider.DependencySlice ||
		kind == provider.DependencyMap {
		candidates = index.lookup(input.Type, providers)
		if len(candidates) != 0 {
			kind = provider.DependencySingle
		}
	}
	if len(candidates) == 0 {
		candidates = index.lookup(input.MatchType(), providers)
	}
	return kind, filterQualified(
		candidates,
		input.Qualifiers,
		providers,
	)
}

func connectCollection(
	result *Result,
	adjacency [][]int,
	consumerIndex int,
	input provider.Dependency,
	kind provider.DependencyKind,
	candidates []int,
) {
	consumer := &result.providers[consumerIndex]
	candidates = orderedCollectionCandidates(candidates, result.providers)
	if kind == provider.DependencyMap {
		if duplicate := duplicateCandidateName(
			candidates,
			result.providers,
		); duplicate != "" {
			result.diagnostics = append(
				result.diagnostics,
				collectionNameDiagnostic(consumer, input, duplicate),
			)
			return
		}
	}
	if scoped := firstScopedCandidate(
		candidates,
		result.providers,
	); scoped >= 0 {
		result.diagnostics = append(
			result.diagnostics,
			scopeDiagnostic(
				consumer,
				input,
				&result.providers[scoped],
			),
		)
		return
	}
	for collectionIndex, dependencyIndex := range candidates {
		appendEdge(
			result,
			adjacency,
			consumerIndex,
			dependencyIndex,
			input,
			kind,
			collectionIndex,
		)
	}
}

func connectSingle(
	result *Result,
	adjacency [][]int,
	consumerIndex int,
	input provider.Dependency,
	kind provider.DependencyKind,
	candidates []int,
) {
	consumer := &result.providers[consumerIndex]
	if len(candidates) == 0 {
		if input.Kind != provider.DependencyOptional {
			result.diagnostics = append(
				result.diagnostics,
				missingDiagnostic(consumer, input),
			)
		}
		return
	}
	candidates = selectedCandidates(candidates, input, result.providers)
	if len(candidates) != 1 {
		result.diagnostics = append(
			result.diagnostics,
			ambiguousDiagnostic(
				consumer,
				input,
				candidates,
				result.providers,
			),
		)
		return
	}
	dependencyIndex := candidates[0]
	if result.providers[dependencyIndex].Scope !=
		sdk.BeanScopeSingleton &&
		input.Kind != provider.DependencyProvider {
		result.diagnostics = append(
			result.diagnostics,
			scopeDiagnostic(
				consumer,
				input,
				&result.providers[dependencyIndex],
			),
		)
		return
	}
	appendEdge(
		result,
		adjacency,
		consumerIndex,
		dependencyIndex,
		input,
		kind,
		0,
	)
}

func selectedCandidates(
	candidates []int,
	input provider.Dependency,
	providers []provider.Provider,
) []int {
	candidates = preferNonFallback(candidates, providers)
	if len(candidates) > 1 {
		candidates = selectPrimary(candidates, providers)
	}
	if len(candidates) > 1 && input.Name != "" {
		candidates = selectByParameterName(
			candidates,
			input.Name,
			providers,
		)
	}
	return candidates
}

func duplicateCandidateName(
	candidates []int,
	providers []provider.Provider,
) string {
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		name := providers[candidate].Name
		if _, duplicate := seen[name]; duplicate {
			return name
		}
		seen[name] = struct{}{}
	}
	return ""
}

func collectionNameDiagnostic(
	consumer *provider.Provider,
	input provider.Dependency,
	name string,
) Diagnostic {
	return Diagnostic{
		Position: preferredPosition(
			input.Position,
			consumer.Position,
		),
		PhysicalPosition: preferredPosition(
			input.PhysicalPosition,
			consumer.PhysicalPosition,
		),
		ProviderID: consumer.SymbolID,
		Kind:       "duplicate-collection-name",
		Message: fmt.Sprintf(
			"%s %s (%s) requests map injection for %s, but multiple matching beans use name %q; assign unique explicit bean names",
			providerRole(consumer),
			providerLabel(consumer),
			consumer.SymbolID,
			dependencyTypeID(input),
			name,
		),
	}
}

func firstScopedCandidate(
	candidates []int,
	providers []provider.Provider,
) int {
	for _, candidate := range candidates {
		if providers[candidate].Scope != sdk.BeanScopeSingleton {
			return candidate
		}
	}
	return -1
}

func scopeDiagnostic(
	consumer *provider.Provider,
	input provider.Dependency,
	dependency *provider.Provider,
) Diagnostic {
	position := preferredPosition(input.Position, consumer.Position)
	physical := preferredPosition(
		input.PhysicalPosition,
		consumer.PhysicalPosition,
	)
	return Diagnostic{
		Position:         position,
		PhysicalPosition: physical,
		ProviderID:       consumer.SymbolID,
		Kind:             "scope-mismatch",
		Message: fmt.Sprintf(
			"%s %s (%s) requests %s directly for %s, but matching bean %q has %s scope; inject bean.Provider[%s] so acquisition and cleanup ownership remain explicit",
			providerRole(consumer),
			providerLabel(consumer),
			consumer.SymbolID,
			dependencyTypeID(input),
			parameterLabel(input),
			dependency.Name,
			dependency.Scope,
			dependencyTypeID(input),
		),
	}
}

func appendEdge(
	result *Result,
	adjacency [][]int,
	consumerIndex int,
	dependencyIndex int,
	input provider.Dependency,
	kind provider.DependencyKind,
	collectionIndex int,
) {
	consumer := &result.providers[consumerIndex]
	dependency := &result.providers[dependencyIndex]
	requiredTypeID := input.TypeID
	if kind != provider.DependencySingle &&
		input.ElementTypeID != "" {
		requiredTypeID = input.ElementTypeID
	}
	result.edges = append(result.edges, Edge{
		consumer:        consumer,
		dependency:      dependency,
		ConsumerID:      consumer.SymbolID,
		DependencyID:    dependency.SymbolID,
		RequiredTypeID:  requiredTypeID,
		ParameterIndex:  input.Index,
		ParameterName:   input.Name,
		DependencyKind:  kind,
		CollectionIndex: collectionIndex,
		Position: preferredPosition(
			input.Position,
			consumer.Position,
		),
		PhysicalPosition: preferredPosition(
			input.PhysicalPosition,
			consumer.PhysicalPosition,
		),
	})
	adjacency[consumerIndex] = append(
		adjacency[consumerIndex],
		dependencyIndex,
	)
}

func orderedCollectionCandidates(
	candidates []int,
	providers []provider.Provider,
) []int {
	result := append([]int(nil), candidates...)
	sort.SliceStable(result, func(i, j int) bool {
		left := &providers[result[i]]
		right := &providers[result[j]]
		if left.Order != right.Order {
			return left.Order < right.Order
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.SymbolID < right.SymbolID
	})
	return result
}

func filterQualified(
	candidates []int,
	qualifiers []string,
	providers []provider.Provider,
) []int {
	if len(qualifiers) == 0 {
		return candidates
	}
	result := make([]int, 0, len(candidates))
	for _, candidate := range candidates {
		item := &providers[candidate]
		matched := true
		for _, qualifier := range qualifiers {
			if qualifier != item.Name &&
				!slices.Contains(item.Aliases, qualifier) &&
				!slices.Contains(item.Qualifiers, qualifier) {
				matched = false
				break
			}
		}
		if matched {
			result = append(result, candidate)
		}
	}
	return result
}

func preferNonFallback(
	candidates []int,
	providers []provider.Provider,
) []int {
	var regular []int
	for _, candidate := range candidates {
		if !providers[candidate].Fallback {
			regular = append(regular, candidate)
		}
	}
	if len(regular) != 0 {
		return regular
	}
	return candidates
}

func selectPrimary(
	candidates []int,
	providers []provider.Provider,
) []int {
	var primary []int
	for _, candidate := range candidates {
		if providers[candidate].Primary {
			primary = append(primary, candidate)
		}
	}
	if len(primary) != 0 {
		return primary
	}
	return candidates
}

func selectByParameterName(
	candidates []int,
	name string,
	providers []provider.Provider,
) []int {
	var result []int
	for _, candidate := range candidates {
		item := &providers[candidate]
		if item.Name == name || slices.Contains(item.Aliases, name) {
			result = append(result, candidate)
		}
	}
	if len(result) != 0 {
		return result
	}
	return candidates
}

type typeIndex map[string][]int

func buildTypeIndex(providers []provider.Provider) typeIndex {
	index := make(typeIndex, len(providers))
	for providerIndex := range providers {
		key := semanticTypeKey(providers[providerIndex].Output)
		index[key] = append(index[key], providerIndex)
		for _, binding := range providers[providerIndex].Interfaces {
			key = semanticTypeKey(binding.Type)
			index[key] = append(index[key], providerIndex)
		}
	}
	for key := range index {
		index[key] = sortedUnique(index[key], providers)
	}
	return index
}

func (index typeIndex) lookup(
	required types.Type,
	providers []provider.Provider,
) []int {
	var result []int
	for _, providerIndex := range index[semanticTypeKey(required)] {
		if types.Identical(required, providers[providerIndex].Output) {
			result = append(result, providerIndex)
			continue
		}
		for _, binding := range providers[providerIndex].Interfaces {
			if types.Identical(required, binding.Type) {
				result = append(result, providerIndex)
				break
			}
		}
	}
	return sortedUnique(result, providers)
}

func semanticTypeKey(value types.Type) string {
	if value == nil {
		return "<invalid>"
	}
	value = types.Unalias(value)
	switch typed := value.(type) {
	case *types.Array:
		return fmt.Sprintf("[%d]%s", typed.Len(), semanticTypeKey(typed.Elem()))
	case *types.Slice:
		return "[]" + semanticTypeKey(typed.Elem())
	case *types.Pointer:
		return "*" + semanticTypeKey(typed.Elem())
	case *types.Map:
		return "map[" + semanticTypeKey(typed.Key()) + "]" + semanticTypeKey(typed.Elem())
	case *types.Chan:
		return fmt.Sprintf("chan%d:%s", typed.Dir(), semanticTypeKey(typed.Elem()))
	default:
		return provider.TypeID(value)
	}
}

func missingDiagnostic(consumer *provider.Provider, input provider.Dependency) Diagnostic {
	position := preferredPosition(input.Position, consumer.Position)
	physical := preferredPosition(input.PhysicalPosition, consumer.PhysicalPosition)
	return Diagnostic{
		Position:         position,
		PhysicalPosition: physical,
		ProviderID:       consumer.SymbolID,
		Kind:             "missing-dependency",
		Message: fmt.Sprintf(
			"%s %s (%s) requires exact type %s for %s%s, but no provider matches the type and requested qualifiers",
			providerRole(consumer),
			providerLabel(consumer),
			consumer.SymbolID,
			dependencyTypeID(input),
			parameterLabel(input),
			qualifierSuffix(input),
		),
	}
}

func ambiguousDiagnostic(
	consumer *provider.Provider,
	input provider.Dependency,
	candidates []int,
	providers []provider.Provider,
) Diagnostic {
	position := preferredPosition(input.Position, consumer.Position)
	physical := preferredPosition(
		input.PhysicalPosition,
		consumer.PhysicalPosition,
	)
	labels := make([]string, len(candidates))
	for index, candidate := range candidates {
		item := &providers[candidate]
		labels[index] = fmt.Sprintf(
			"%s (%s) at %s",
			providerLabel(item),
			item.SymbolID,
			renderPosition(item.Position),
		)
	}
	return Diagnostic{
		Position:         position,
		PhysicalPosition: physical,
		ProviderID:       consumer.SymbolID,
		Kind:             "ambiguous-dependency",
		Message: fmt.Sprintf(
			"%s %s (%s) requires type %s for %s%s, but multiple explicit providers match after qualifier, fallback, primary, and parameter-name selection: %s",
			providerRole(consumer),
			providerLabel(consumer),
			consumer.SymbolID,
			dependencyTypeID(input),
			parameterLabel(input),
			qualifierSuffix(input),
			strings.Join(labels, ", "),
		),
	}
}

func dependencyTypeID(input provider.Dependency) string {
	if input.ElementTypeID != "" {
		return input.ElementTypeID
	}
	return input.TypeID
}

func qualifierSuffix(input provider.Dependency) string {
	if len(input.Qualifiers) == 0 {
		return ""
	}
	return " with qualifiers [" +
		strings.Join(input.Qualifiers, ", ") + "]"
}

func providerRole(item *provider.Provider) string {
	switch item.Source {
	case provider.SourceBean:
		return "@Bean provider"
	case provider.SourceStarter:
		return "starter entrypoint"
	case provider.SourceAutoConfiguration:
		return "auto-configuration factory"
	case provider.SourceStereotype:
		return "stereotype bean"
	case provider.SourceConfiguration:
		return "configuration provider"
	case provider.SourceEvent:
		return "event topic provider"
	}
	return "provider"
}

func parameterLabel(input provider.Dependency) string {
	if input.Name == "" {
		return fmt.Sprintf("parameter %d", input.Index)
	}
	return fmt.Sprintf("parameter %d %q", input.Index, input.Name)
}

func providerLabel(item *provider.Provider) string {
	if item.Symbol.DisplayLabel != "" {
		return item.Symbol.DisplayLabel
	}
	if item.Name != "" {
		return item.Name
	}
	return item.SymbolID
}

func preferredPosition(primary, fallback token.Position) token.Position {
	if primary.Filename != "" || primary.Offset != 0 || primary.Line != 0 {
		return primary
	}
	return fallback
}

func sortedUnique(values []int, providers []provider.Provider) []int {
	sort.Slice(values, func(i, j int) bool {
		return providers[values[i]].SymbolID < providers[values[j]].SymbolID
	})
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func stronglyConnected(adjacency [][]int, providers []provider.Provider) [][]int {
	indices := make([]int, len(adjacency))
	lowlinks := make([]int, len(adjacency))
	onStack := make([]bool, len(adjacency))
	for index := range indices {
		indices[index] = -1
	}
	stack := make([]int, 0, len(adjacency))
	nextIndex := 0
	var components [][]int
	var visit func(int)
	visit = func(node int) {
		indices[node], lowlinks[node] = nextIndex, nextIndex
		nextIndex++
		stack = append(stack, node)
		onStack[node] = true
		for _, dependency := range adjacency[node] {
			if indices[dependency] == -1 {
				visit(dependency)
				if lowlinks[dependency] < lowlinks[node] {
					lowlinks[node] = lowlinks[dependency]
				}
			} else if onStack[dependency] && indices[dependency] < lowlinks[node] {
				lowlinks[node] = indices[dependency]
			}
		}
		if lowlinks[node] != indices[node] {
			return
		}
		var component []int
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, last)
			if last == node {
				break
			}
		}
		sort.Slice(component, func(i, j int) bool {
			return providers[component[i]].SymbolID < providers[component[j]].SymbolID
		})
		components = append(components, component)
	}

	order := make([]int, len(adjacency))
	for index := range order {
		order[index] = index
	}
	sort.Slice(order, func(i, j int) bool {
		return providers[order[i]].SymbolID < providers[order[j]].SymbolID
	})
	for _, node := range order {
		if indices[node] == -1 {
			visit(node)
		}
	}
	sort.Slice(components, func(i, j int) bool {
		return providers[components[i][0]].SymbolID < providers[components[j][0]].SymbolID
	})
	return components
}

func cycleDiagnostic(component []int, adjacency [][]int, providers []provider.Provider) Diagnostic {
	path := representativeCycle(component, adjacency)
	pathIDs := make([]string, len(path))
	for index, node := range path {
		pathIDs[index] = providers[node].SymbolID
	}
	context := make([]string, len(component))
	for index, node := range component {
		context[index] = fmt.Sprintf("%s at %s", providerLabel(&providers[node]), renderPosition(providers[node].Position))
	}
	first := &providers[component[0]]
	return Diagnostic{
		Position:         first.Position,
		PhysicalPosition: first.PhysicalPosition,
		ProviderID:       first.SymbolID,
		Kind:             "cycle",
		Message: fmt.Sprintf(
			"provider dependency cycle: %s; component providers: %s",
			strings.Join(pathIDs, " -> "), strings.Join(context, ", "),
		),
	}
}

func representativeCycle(component []int, adjacency [][]int) []int {
	start := component[0]
	if containsIndex(adjacency[start], start) {
		return []int{start, start}
	}
	members := make(map[int]bool, len(component))
	for _, node := range component {
		members[node] = true
	}
	visited := map[int]bool{start: true}
	path := []int{start}
	var search func(int) bool
	search = func(node int) bool {
		for _, next := range adjacency[node] {
			if !members[next] {
				continue
			}
			if next == start {
				path = append(path, start)
				return true
			}
			if visited[next] {
				continue
			}
			visited[next] = true
			path = append(path, next)
			if search(next) {
				return true
			}
			path = path[:len(path)-1]
			delete(visited, next)
		}
		return false
	}
	if search(start) {
		return path
	}
	// Tarjan guarantees a path for an SCC; retain deterministic context if a
	// future representation violates that assumption.
	fallback := append([]int(nil), component...)
	return append(fallback, component[0])
}

type constructionStats struct {
	readyComparisons int
}

type readyPriorityQueue struct {
	values      []int
	providers   []provider.Provider
	comparisons *int
}

func (queue *readyPriorityQueue) Len() int { return len(queue.values) }

func (queue *readyPriorityQueue) Less(i, j int) bool {
	if queue.comparisons != nil {
		*queue.comparisons++
	}
	return queue.providers[queue.values[i]].SymbolID < queue.providers[queue.values[j]].SymbolID
}

func (queue *readyPriorityQueue) Swap(i, j int) {
	queue.values[i], queue.values[j] = queue.values[j], queue.values[i]
}

func (queue *readyPriorityQueue) Push(value any) {
	index, ok := value.(int)
	if !ok {
		panic(fmt.Sprintf("graph priority queue received %T, want int", value))
	}
	queue.values = append(queue.values, index)
}

func (queue *readyPriorityQueue) Pop() any {
	last := len(queue.values) - 1
	value := queue.values[last]
	queue.values = queue.values[:last]
	return value
}

func constructionOrder(adjacency [][]int, providers []provider.Provider) []*provider.Provider {
	order, _ := constructionOrderWithStats(adjacency, providers)
	return order
}

func constructionOrderWithStats(adjacency [][]int, providers []provider.Provider) ([]*provider.Provider, constructionStats) {
	remaining := make([]int, len(providers))
	consumers := make([][]int, len(providers))
	for consumer, dependencies := range adjacency {
		remaining[consumer] = len(dependencies)
		for _, dependency := range dependencies {
			consumers[dependency] = append(consumers[dependency], consumer)
		}
	}
	for index := range consumers {
		consumers[index] = sortedUnique(consumers[index], providers)
	}

	stats := constructionStats{}
	ready := &readyPriorityQueue{
		values:      make([]int, 0, len(providers)),
		providers:   providers,
		comparisons: &stats.readyComparisons,
	}
	for index, count := range remaining {
		if count == 0 {
			ready.values = append(ready.values, index)
		}
	}
	heap.Init(ready)

	order := make([]*provider.Provider, 0, len(providers))
	for ready.Len() != 0 {
		value := heap.Pop(ready)
		node, ok := value.(int)
		if !ok {
			panic(fmt.Sprintf("graph priority queue returned %T, want int", value))
		}
		order = append(order, &providers[node])
		for _, consumer := range consumers[node] {
			remaining[consumer]--
			if remaining[consumer] == 0 {
				heap.Push(ready, consumer)
			}
		}
	}
	return order, stats
}

func containsIndex(values []int, target int) bool {
	return slices.Contains(values, target)
}

func renderPosition(position token.Position) string {
	filename := position.Filename
	if filename == "" {
		filename = "<unknown>"
	}
	line, column := position.Line, position.Column
	if line <= 0 {
		line = 1
	}
	if column <= 0 {
		column = 1
	}
	return fmt.Sprintf("%s:%d:%d", filename, line, column)
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.ProviderID != right.ProviderID {
			return left.ProviderID < right.ProviderID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Message != right.Message {
			return left.Message < right.Message
		}
		if left.PhysicalPosition.Filename != right.PhysicalPosition.Filename {
			return left.PhysicalPosition.Filename < right.PhysicalPosition.Filename
		}
		return left.PhysicalPosition.Offset < right.PhysicalPosition.Offset
	})
}
