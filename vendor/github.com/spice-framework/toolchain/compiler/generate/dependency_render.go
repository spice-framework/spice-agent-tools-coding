package generate

import (
	"fmt"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/application"
	compilerasync "github.com/spice-framework/toolchain/compiler/async"
	compilercache "github.com/spice-framework/toolchain/compiler/cache"
	"github.com/spice-framework/toolchain/compiler/controller"
	compilerevent "github.com/spice-framework/toolchain/compiler/event"
	"github.com/spice-framework/toolchain/compiler/provider"
)

func importAliases(
	providers []provider.Provider,
	controllers []controller.Controller,
	asyncTasks []compilerasync.Task,
	events []compilerevent.Topic,
	caches []compilercache.Boundary,
	features commandFeatures,
) map[string]string {
	aliases := map[string]string{
		"context":     "context",
		"flag":        "flag",
		"fmt":         "fmt",
		"io":          "io",
		"log/slog":    "slog",
		"os":          "os",
		"os/signal":   "signal",
		"syscall":     "syscall",
		"time":        "time",
		configPath:    "spiceconfig",
		lifecyclePath: "spicelifecycle",
	}
	if features.hasMux {
		aliases["net/http"] = "http"
		aliases[webPath] = "spiceweb"
	}
	addViewImportAlias(aliases, controllers)
	if features.management {
		aliases[managementPath] = "spicemanagement"
	}
	if features.logging {
		aliases[observabilityPath] = "spiceobservability"
	}
	if features.authorization {
		aliases[securityPath] = "spicesecurity"
	}
	if features.scheduling {
		aliases[schedulePath] = "spiceschedule"
	}
	if features.asynchronous {
		aliases[asyncPath] = "spiceasync"
	}
	if features.transactions {
		aliases["database/sql"] = "sql"
		aliases[dataPath] = "spicedata"
	}
	if features.events {
		aliases[eventPath] = "spiceevent"
	}
	if features.caching {
		aliases[cachePath] = "spicecache"
	}
	if usesBeanHandles(providers) {
		aliases[beanPath] = "spicebean"
	}
	used := map[string]struct{}{
		"Application":               {},
		"ApplicationOptions":        {},
		"CommandOptions":            {},
		"ConfigurationSchema":       {},
		"ExitFailure":               {},
		"ExitSuccess":               {},
		"ExitUsage":                 {},
		"Main":                      {},
		"NewApplication":            {},
		"NewApplicationWithOptions": {},
		"RunCommand":                {},
		"TargetID":                  {},
		"context":                   {},
		"flag":                      {},
		"fmt":                       {},
		"http":                      {},
		"io":                        {},
		"os":                        {},
		"signal":                    {},
		"slog":                      {},
		"sql":                       {},
		"strings":                   {},
		"spiceconfig":               {},
		"spiceasync":                {},
		"spicecache":                {},
		"spicebean":                 {},
		"spicedata":                 {},
		"spiceevent":                {},
		"spiceintercept":            {},
		"spicelifecycle":            {},
		"spicemanagement":           {},
		"spiceobservability":        {},
		"spiceschedule":             {},
		"spicesecurity":             {},
		"spiceview":                 {},
		"spiceweb":                  {},
		"syscall":                   {},
		"time":                      {},
	}
	names := importNames(
		providers,
		controllers,
		asyncTasks,
		events,
		caches,
		aliases,
	)
	paths := make([]string, 0, len(names))
	for importPath := range names {
		paths = append(paths, importPath)
	}
	sort.Strings(paths)
	for _, importPath := range paths {
		base := names[importPath]
		alias := base
		for suffix := 2; ; suffix++ {
			if _, exists := used[alias]; !exists {
				break
			}
			alias = base + strconv.Itoa(suffix)
		}
		used[alias] = struct{}{}
		aliases[importPath] = alias
	}
	return aliases
}

func addViewImportAlias(
	aliases map[string]string,
	controllers []controller.Controller,
) {
	if hasViewRoutes(controllers) {
		aliases[viewPath] = "spiceview"
	}
}

func hasViewRoutes(controllers []controller.Controller) bool {
	for _, item := range controllers {
		for _, route := range item.Routes() {
			if route.View {
				return true
			}
		}
	}
	return false
}

func importNames(
	providers []provider.Provider,
	controllers []controller.Controller,
	asyncTasks []compilerasync.Task,
	events []compilerevent.Topic,
	caches []compilercache.Boundary,
	aliases map[string]string,
) map[string]string {
	names := make(map[string]string)
	addProviderImportNames(names, aliases, providers)
	for _, item := range controllers {
		for _, route := range item.Routes() {
			for _, binding := range route.Bindings() {
				addTypeImportName(names, aliases, binding.Type)
			}
		}
	}
	for _, task := range asyncTasks {
		for _, parameter := range task.Parameters() {
			addTypeImportName(names, aliases, parameter.Type)
		}
	}
	for _, topic := range events {
		addTypeImportName(names, aliases, topic.Payload)
	}
	for _, boundary := range caches {
		addTypeImportName(names, aliases, boundary.Key)
		addTypeImportName(names, aliases, boundary.Value)
	}
	return names
}

func addProviderImportNames(
	names map[string]string,
	aliases map[string]string,
	providers []provider.Provider,
) {
	for _, item := range providers {
		addTypeImportName(names, aliases, item.Output)
		for _, dependency := range item.Dependencies {
			if dependency.Kind == provider.DependencySingle {
				continue
			}
			addTypeImportName(
				names,
				aliases,
				dependency.Type,
			)
		}
	}
}

func addTypeImportName(names, aliases map[string]string, value types.Type) {
	switch typed := value.(type) {
	case *types.Named:
		addTypeObjectImportName(names, aliases, typed.Obj())
		addTypeArgumentImportNames(names, aliases, typed.TypeArgs())
	case *types.Alias:
		addTypeObjectImportName(names, aliases, typed.Obj())
		addTypeArgumentImportNames(names, aliases, typed.TypeArgs())
	case *types.Pointer:
		addTypeImportName(names, aliases, typed.Elem())
	case *types.Slice:
		addTypeImportName(names, aliases, typed.Elem())
	case *types.Array:
		addTypeImportName(names, aliases, typed.Elem())
	case *types.Map:
		addTypeImportName(names, aliases, typed.Key())
		addTypeImportName(names, aliases, typed.Elem())
	case *types.Chan:
		addTypeImportName(names, aliases, typed.Elem())
	}
}

func addTypeArgumentImportNames(
	names, aliases map[string]string,
	arguments *types.TypeList,
) {
	if arguments == nil {
		return
	}
	for argument := range arguments.Types() {
		addTypeImportName(names, aliases, argument)
	}
}

func addTypeObjectImportName(
	names, aliases map[string]string,
	object *types.TypeName,
) {
	if object == nil || object.Pkg() == nil {
		return
	}
	importPath := object.Pkg().Path()
	if _, fixed := aliases[importPath]; fixed {
		return
	}
	names[importPath] = object.Pkg().Name()
}

func dependencyVariables(
	model application.Model,
	providers []provider.Provider,
	aliases map[string]string,
) (map[string][]string, error) {
	edges := make(map[string][]graphEdge)
	for _, edge := range model.Edges() {
		key := dependencyKey(edge.ConsumerID, edge.ParameterIndex)
		edges[key] = append(edges[key], graphEdge{
			providerID: edge.DependencyID,
			index:      edge.CollectionIndex,
			kind:       edge.DependencyKind,
		})
	}
	variables, _ := targetProviderVariables(providers)
	result := make(map[string][]string, len(providers))
	for _, item := range providers {
		inputs := make([]string, len(item.Dependencies))
		for _, dependency := range item.Dependencies {
			selected := edges[dependencyKey(
				item.SymbolID,
				dependency.Index,
			)]
			if len(selected) == 0 &&
				dependency.Kind != provider.DependencySlice &&
				dependency.Kind != provider.DependencyMap &&
				dependency.Kind != provider.DependencyOptional {
				return nil, fmt.Errorf(
					"provider %s parameter %d has no validated graph edge",
					item.SymbolID,
					dependency.Index,
				)
			}
			sort.SliceStable(selected, func(i, j int) bool {
				return selected[i].index < selected[j].index
			})
			selectedVariables := make([]string, len(selected))
			selectedProviders := make([]provider.Provider, len(selected))
			for selectedIndex, edge := range selected {
				variable, ok := variables[edge.providerID]
				if !ok {
					return nil, fmt.Errorf(
						"provider %s parameter %d references unknown provider %s",
						item.SymbolID,
						dependency.Index,
						edge.providerID,
					)
				}
				selectedVariables[selectedIndex] = variable
				selectedProviders[selectedIndex] = providerByID(providers, edge.providerID)
			}
			effective := dependency
			if len(selected) != 0 &&
				selected[0].kind == provider.DependencySingle {
				effective.Kind = provider.DependencySingle
				effective.Element = nil
				effective.ElementTypeID = ""
			}
			inputs[dependency.Index] = dependencyExpression(
				effective,
				selectedVariables,
				selectedProviders,
				aliases,
			)
		}
		result[item.SymbolID] = inputs
	}
	return result, nil
}

type graphEdge struct {
	providerID string
	index      int
	kind       provider.DependencyKind
}

func dependencyExpression(
	dependency provider.Dependency,
	variables []string,
	selected []provider.Provider,
	aliases map[string]string,
) string {
	elementType := renderedType(dependency.MatchType(), aliases)
	switch dependency.Kind {
	case provider.DependencySingle:
		if len(variables) == 0 {
			return ""
		}
		return variables[0]
	case provider.DependencySlice:
		return renderedType(dependency.Type, aliases) +
			"{" + strings.Join(variables, ", ") + "}"
	case provider.DependencyMap:
		entries := make([]string, len(variables))
		for index, variable := range variables {
			entries[index] = strconv.Quote(selected[index].Name) +
				": " + variable
		}
		return renderedType(dependency.Type, aliases) +
			"{" + strings.Join(entries, ", ") + "}"
	case provider.DependencyOptional:
		if len(variables) == 0 {
			return "spicebean.None[" + elementType + "]()"
		}
		return "spicebean.Some[" + elementType + "](" +
			variables[0] + ")"
	case provider.DependencyLazy:
		return "spicebean.NewLazy(func(context.Context) (" +
			elementType + ", error) { return " +
			variables[0] + ", nil })"
	case provider.DependencyProvider:
		if len(selected) != 0 &&
			selected[0].Scope != sdk.BeanScopeSingleton {
			return variables[0]
		}
		return "spicebean.NewProvider(func(context.Context) (" +
			elementType +
			", spicelifecycle.Cleanup, error) { return " +
			variables[0] + ", nil, nil })"
	}
	return ""
}

func providerByID(
	providers []provider.Provider,
	symbolID string,
) provider.Provider {
	for _, item := range providers {
		if item.SymbolID == symbolID {
			return item
		}
	}
	return provider.Provider{}
}

func usesBeanHandles(providers []provider.Provider) bool {
	for _, item := range providers {
		if item.Scope != sdk.BeanScopeSingleton {
			return true
		}
		for _, dependency := range item.Dependencies {
			switch dependency.Kind {
			case provider.DependencyOptional,
				provider.DependencyLazy,
				provider.DependencyProvider:
				return true
			case provider.DependencySingle,
				provider.DependencySlice,
				provider.DependencyMap:
			}
		}
	}
	return false
}
