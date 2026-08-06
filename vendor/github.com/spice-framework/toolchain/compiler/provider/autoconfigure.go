package provider

import (
	"fmt"
	"go/types"
	"slices"
	"sort"
	"strings"
)

// AutoConfigurationStatus describes why one imported library default was or
// was not incorporated into the application provider graph.
type AutoConfigurationStatus string

const (
	AutoConfigurationSelected AutoConfigurationStatus = "selected"
	AutoConfigurationReplaced AutoConfigurationStatus = "replaced"
	AutoConfigurationInactive AutoConfigurationStatus = "inactive"
)

// AutoConfigurationDecision is one deterministic default-provider outcome.
type AutoConfigurationDecision struct {
	Provider Provider
	Status   AutoConfigurationStatus
	Reason   string
}

// SelectAutoConfiguration prunes library defaults before application graph
// construction. Application-owned exact outputs win, and defaults whose
// required inputs are unavailable back off without being constructed.
func SelectAutoConfiguration(
	primary Catalog,
	defaults Catalog,
) (Catalog, []AutoConfigurationDecision) {
	decisions := make([]AutoConfigurationDecision, 0, len(defaults.providers))
	if len(defaults.diagnostics) != 0 {
		return defaults, decisions
	}

	available := primary.Providers()
	pending := defaults.Providers()
	eligible := pending[:0]
	for _, candidate := range pending {
		if replacement, found := autoConfigurationReplacement(
			primary.providers,
			pending,
			candidate,
		); found {
			decisions = append(decisions, AutoConfigurationDecision{
				Provider: candidate,
				Status:   AutoConfigurationReplaced,
				Reason: fmt.Sprintf(
					"application bean %s replaces default bean %s with exact type %s",
					replacement.Name,
					candidate.Name,
					candidate.OutputTypeID,
				),
			})
			continue
		}
		eligible = append(eligible, candidate)
	}
	pending = eligible
	for len(pending) != 0 {
		var next []Provider
		progress := false
		for _, candidate := range pending {
			missing := missingRequiredDependencies(candidate, available)
			if len(missing) != 0 {
				next = append(next, candidate)
				continue
			}
			available = append(available, candidate)
			decisions = append(decisions, AutoConfigurationDecision{
				Provider: candidate,
				Status:   AutoConfigurationSelected,
				Reason:   "all required bean inputs are available",
			})
			progress = true
		}
		if !progress {
			for _, candidate := range next {
				missing := missingRequiredDependencies(candidate, available)
				decisions = append(decisions, AutoConfigurationDecision{
					Provider: candidate,
					Status:   AutoConfigurationInactive,
					Reason: "missing required bean inputs: " +
						strings.Join(missing, ", "),
				})
			}
			break
		}
		pending = next
	}

	selected := Catalog{}
	for _, decision := range decisions {
		if decision.Status == AutoConfigurationSelected {
			selected.providers = append(selected.providers, decision.Provider)
		}
	}
	sort.Slice(selected.providers, func(i, j int) bool {
		return selected.providers[i].SymbolID < selected.providers[j].SymbolID
	})
	normalizeProviderMetadata(selected.providers)
	selected.diagnostics = append(
		beanIdentityDiagnostics(selected.providers),
		nonSelectableDuplicateDiagnostics(selected.providers)...,
	)
	sortDiagnostics(selected.diagnostics)
	sort.Slice(decisions, func(i, j int) bool {
		return decisions[i].Provider.SymbolID < decisions[j].Provider.SymbolID
	})
	return selected, decisions
}

func autoConfigurationReplacement(
	primary []Provider,
	defaults []Provider,
	candidate Provider,
) (Provider, bool) {
	if exactOutputCount(defaults, candidate.Output) == 1 {
		return exactOutputProvider(primary, candidate.Output)
	}
	candidateNames := providerNames(candidate)
	for _, item := range primary {
		if !types.Identical(item.Output, candidate.Output) ||
			!namesIntersect(candidateNames, providerNames(item)) {
			continue
		}
		return item, true
	}
	return Provider{}, false
}

func exactOutputCount(providers []Provider, output types.Type) int {
	count := 0
	for _, item := range providers {
		if types.Identical(item.Output, output) {
			count++
		}
	}
	return count
}

func providerNames(item Provider) []string {
	return append([]string{item.Name}, item.Aliases...)
}

func namesIntersect(left, right []string) bool {
	for _, leftName := range left {
		for _, rightName := range right {
			if leftName != "" && leftName == rightName {
				return true
			}
		}
	}
	return false
}

func exactOutputProvider(
	providers []Provider,
	output types.Type,
) (Provider, bool) {
	for _, item := range providers {
		if types.Identical(item.Output, output) {
			return item, true
		}
	}
	return Provider{}, false
}

func missingRequiredDependencies(
	candidate Provider,
	available []Provider,
) []string {
	var missing []string
	for _, dependency := range candidate.Dependencies {
		switch dependency.Kind {
		case DependencySlice, DependencyMap, DependencyOptional:
			continue
		case DependencySingle, DependencyLazy, DependencyProvider:
		}
		if providerMatchesType(available, dependency.MatchType()) {
			continue
		}
		typeID := dependency.ElementTypeID
		if typeID == "" {
			typeID = dependency.TypeID
		}
		missing = append(missing, typeID)
	}
	slices.Sort(missing)
	return slices.Compact(missing)
}

func providerMatchesType(providers []Provider, target types.Type) bool {
	for _, item := range providers {
		if types.Identical(item.Output, target) {
			return true
		}
		for _, binding := range item.Interfaces {
			if types.Identical(binding.Type, target) {
				return true
			}
		}
	}
	return false
}
