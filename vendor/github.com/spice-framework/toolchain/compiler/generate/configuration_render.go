package generate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/types"
	"sort"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation/sdk"
	runtimeconfig "github.com/spice-framework/spice/config"
	compilercache "github.com/spice-framework/toolchain/compiler/cache"
	"github.com/spice-framework/toolchain/compiler/configuration"
	"github.com/spice-framework/toolchain/compiler/provider"
)

func writeApplicationOptions(
	source *bytes.Buffer,
	features commandFeatures,
	componentFields []generatedComponentField,
	routeInterceptorFields []generatedRouteInterceptorField,
) {
	source.WriteString("type ApplicationOptions struct {\n")
	if hasOverridableProviders(componentFields) {
		source.WriteString("\tOverrides BeanOverrides\n")
	}
	source.WriteString("\tProfiles []string\n")
	source.WriteString("\tSources []spiceconfig.Source\n")
	source.WriteString("\tAllowUnknownConfiguration bool\n")
	if features.hasMux {
		source.WriteString("\tErrorMapper spiceweb.ErrorMapper\n")
		source.WriteString("\tMaxRequestBodyBytes int64\n")
		source.WriteString("\tHTTPObservers []spiceweb.HTTPObserver\n")
		source.WriteString("\tMiddleware []spiceweb.Middleware\n")
		if len(routeInterceptorFields) != 0 {
			source.WriteString("\tInterceptors RouteInterceptors\n")
		}
		if features.requestScope {
			source.WriteString("\tScopeErrorHandler spicebean.ScopeErrorHandler\n")
		}
	}
	if features.authorization {
		source.WriteString("\tAuthorizationObservers []spicesecurity.Observer\n")
		source.WriteString("\tAuthorizationWriteFailure spicesecurity.WriteFailure\n")
	}
	if features.logging {
		source.WriteString("\tLogger *slog.Logger\n")
	}
	if features.scheduling {
		source.WriteString("\tScheduleContext context.Context\n")
		source.WriteString("\tScheduleWaiter spiceschedule.Waiter\n")
		source.WriteString("\tScheduleObservers []spiceschedule.Observer\n")
	}
	if features.asynchronous {
		source.WriteString("\tAsyncContext context.Context\n")
		source.WriteString("\tAsyncObservers []spiceasync.Observer\n")
	}
	if features.events {
		source.WriteString("\tEventObservers []spiceevent.Observer\n")
	}
	if features.caching {
		source.WriteString("\tCacheClock func() time.Time\n")
		source.WriteString("\tCacheObservers []spicecache.Observer\n")
	}
	source.WriteString("\tObservers []spicelifecycle.Observer\n")
	source.WriteString("}\n\n")
}

func writeRequestScopeSetup(
	source *bytes.Buffer,
	features commandFeatures,
) {
	if !features.hasMux || !features.requestScope {
		return
	}
	source.WriteString("\trequestScopedHandler, err := spicebean.RequestScopeMiddleware(application.handler, options.ScopeErrorHandler)\n")
	source.WriteString("\tif err != nil {\n")
	source.WriteString("\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(\"configure request bean scope: %w\", err))\n")
	source.WriteString("\t}\n")
	source.WriteString("\tapplication.handler = requestScopedHandler\n")
}

func hasProviderScope(
	providers []provider.Provider,
	scope sdk.BeanScope,
) bool {
	for _, item := range providers {
		if item.Scope == scope {
			return true
		}
	}
	return false
}

func writeConfigurationAPI(
	source *bytes.Buffer,
	configTypes []configuration.Type,
	caches []compilercache.Boundary,
	asynchronous bool,
) {
	source.WriteString("func ConfigurationSchema() (spiceconfig.Schema, error) {\n")
	source.WriteString("\treturn spiceconfig.NewSchema(\n")
	for _, configType := range configTypes {
		for _, field := range configType.Fields() {
			source.WriteString("\t\tspiceconfig.Property{\n")
			fmt.Fprintf(source, "\t\t\tKey: %s,\n", strconv.Quote(field.Key))
			fmt.Fprintf(source, "\t\t\tKind: %s,\n", configurationKindName(field.Kind))
			if field.Module != "" {
				fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(field.Module))
			}
			if field.Environment != "" {
				fmt.Fprintf(source, "\t\t\tEnvironment: %s,\n", strconv.Quote(field.Environment))
			}
			if field.HasDefault {
				fmt.Fprintf(source, "\t\t\tDefault: %s,\n", strconv.Quote(field.Default))
				source.WriteString("\t\t\tHasDefault: true,\n")
			}
			if field.Required {
				source.WriteString("\t\t\tRequired: true,\n")
			}
			if field.Secret {
				source.WriteString("\t\t\tSecret: true,\n")
			}
			source.WriteString("\t\t},\n")
		}
	}
	for _, boundary := range caches {
		writeCacheConfigurationProperties(source, boundary)
	}
	if asynchronous {
		source.WriteString("\t\tspiceconfig.Property{\n")
		fmt.Fprintf(
			source,
			"\t\t\tKey: %s,\n",
			strconv.Quote(asyncConcurrencyKey),
		)
		source.WriteString("\t\t\tKind: spiceconfig.KindInteger,\n")
		source.WriteString("\t\t\tDescription: \"Maximum concurrent generated asynchronous tasks\",\n")
		source.WriteString("\t\t\tEnvironment: \"SPICE_ASYNC_MAX_CONCURRENCY\",\n")
		source.WriteString("\t\t\tDefault: \"16\",\n")
		source.WriteString("\t\t\tHasDefault: true,\n")
		source.WriteString("\t\t},\n")
	}
	source.WriteString("\t\tspiceconfig.Property{\n")
	fmt.Fprintf(source, "\t\t\tKey: %s,\n", strconv.Quote(shutdownConfigurationKey))
	source.WriteString("\t\t\tKind: spiceconfig.KindDuration,\n")
	source.WriteString("\t\t\tEnvironment: \"SPICE_SHUTDOWN_TIMEOUT\",\n")
	source.WriteString("\t\t\tDefault: \"10s\",\n")
	source.WriteString("\t\t\tHasDefault: true,\n")
	source.WriteString("\t\t},\n")
	source.WriteString("\t)\n")
	source.WriteString("}\n\n")
}

func writeCacheConfigurationProperties(
	source *bytes.Buffer,
	boundary compilercache.Boundary,
) {
	source.WriteString("\t\tspiceconfig.Property{\n")
	fmt.Fprintf(
		source,
		"\t\t\tKey: %s,\n",
		strconv.Quote(cacheCapacityKey(boundary.CacheName)),
	)
	source.WriteString("\t\t\tKind: spiceconfig.KindInteger,\n")
	fmt.Fprintf(
		source,
		"\t\t\tDescription: %s,\n",
		strconv.Quote("Maximum entries for cache "+boundary.CacheName),
	)
	fmt.Fprintf(
		source,
		"\t\t\tModule: %s,\n",
		strconv.Quote(boundary.Module),
	)
	fmt.Fprintf(
		source,
		"\t\t\tEnvironment: %s,\n",
		strconv.Quote(cacheEnvironment(boundary.CacheName, "CAPACITY")),
	)
	source.WriteString("\t\t\tDefault: \"256\",\n")
	source.WriteString("\t\t\tHasDefault: true,\n")
	source.WriteString("\t\t},\n")
	source.WriteString("\t\tspiceconfig.Property{\n")
	fmt.Fprintf(
		source,
		"\t\t\tKey: %s,\n",
		strconv.Quote(cacheTTLKey(boundary.CacheName)),
	)
	source.WriteString("\t\t\tKind: spiceconfig.KindDuration,\n")
	fmt.Fprintf(
		source,
		"\t\t\tDescription: %s,\n",
		strconv.Quote("Entry lifetime for cache "+boundary.CacheName),
	)
	fmt.Fprintf(
		source,
		"\t\t\tModule: %s,\n",
		strconv.Quote(boundary.Module),
	)
	fmt.Fprintf(
		source,
		"\t\t\tEnvironment: %s,\n",
		strconv.Quote(cacheEnvironment(boundary.CacheName, "TTL")),
	)
	source.WriteString("\t\t\tDefault: \"5m\",\n")
	source.WriteString("\t\t\tHasDefault: true,\n")
	source.WriteString("\t\t},\n")
}

func cacheCapacityKey(name string) string {
	return "spice.cache." + name + ".capacity"
}

func cacheTTLKey(name string) string {
	return "spice.cache." + name + ".ttl"
}

func cacheEnvironment(name, suffix string) string {
	replacer := strings.NewReplacer(".", "_", "-", "_")
	return "SPICE_CACHE_" +
		strings.ToUpper(replacer.Replace(name)) +
		"_" + suffix
}

func configurationKindName(kind runtimeconfig.Kind) string {
	switch kind {
	case runtimeconfig.KindString:
		return "spiceconfig.KindString"
	case runtimeconfig.KindBoolean:
		return "spiceconfig.KindBoolean"
	case runtimeconfig.KindInteger:
		return "spiceconfig.KindInteger"
	case runtimeconfig.KindDuration:
		return "spiceconfig.KindDuration"
	default:
		return strconv.Quote(string(kind))
	}
}

func writeConfigurationResolution(source *bytes.Buffer, target Target) {
	source.WriteString("\tconfigurationSchema, err := ConfigurationSchema()\n")
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, fmt.Errorf(%s, err)\n",
		strconv.Quote("construct configuration schema for application "+target.ID+": %w"),
	)
	source.WriteString("\t}\n")
	source.WriteString("\tconfigurationSnapshot, err := spiceconfig.Resolve(\n")
	source.WriteString("\t\tctx,\n")
	source.WriteString("\t\tconfigurationSchema,\n")
	source.WriteString("\t\tspiceconfig.Options{\n")
	source.WriteString("\t\t\tProfiles: options.Profiles,\n")
	source.WriteString("\t\t\tAllowUnknown: options.AllowUnknownConfiguration,\n")
	source.WriteString("\t\t},\n")
	source.WriteString("\t\toptions.Sources...,\n")
	source.WriteString("\t)\n")
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, fmt.Errorf(%s, err)\n",
		strconv.Quote("resolve configuration for application "+target.ID+": %w"),
	)
	source.WriteString("\t}\n")
	fmt.Fprintf(
		source,
		"\tapplication.shutdownTimeout, err = configurationSnapshot.Duration(%s)\n",
		strconv.Quote(shutdownConfigurationKey),
	)
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
		strconv.Quote("decode shutdown timeout for application "+target.ID+": %w"),
	)
	source.WriteString("\t}\n")
	source.WriteString("\tif application.shutdownTimeout <= 0 {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s))\n",
		strconv.Quote("decode shutdown timeout for application "+target.ID+": duration must be positive"),
	)
	source.WriteString("\t}\n")
}

func writeSourceUnitConfigurationBinder(
	source *bytes.Buffer,
	configType configuration.Type,
	aliases map[string]string,
	configAlias string,
	fmtAlias string,
) {
	functionName := generatedConfigurationFunction(configType)
	outputType := renderedTypeInPackage(configType.Type, aliases)
	fmt.Fprintf(
		source,
		"// %s binds the validated configuration declared by %s.\n",
		functionName,
		configType.SymbolID,
	)
	fmt.Fprintf(
		source,
		"func %s(configurationSnapshot %s.Snapshot) (%s, error) {\n",
		functionName,
		configAlias,
		outputType,
	)
	fmt.Fprintf(source, "\tvalue := %s{}\n", outputType)
	for _, field := range configType.Fields() {
		source.WriteString("\tif _, configured := configurationSnapshot.Lookup(")
		source.WriteString(strconv.Quote(field.Key))
		source.WriteString("); configured {\n")
		fmt.Fprintf(
			source,
			"\t\trawValue, valueErr := configurationSnapshot.%s(%s)\n",
			configurationAccessor(field.Kind),
			strconv.Quote(field.Key),
		)
		source.WriteString("\t\tif valueErr != nil {\n")
		fmt.Fprintf(
			source,
			"\t\t\treturn %s{}, %s.Errorf(%s, valueErr)\n",
			outputType,
			fmtAlias,
			strconv.Quote(
				"decode configuration property "+field.Key+" for "+
					configType.TypeID+"."+field.Name+": %w",
			),
		)
		source.WriteString("\t\t}\n")
		fmt.Fprintf(
			source,
			"\t\tconvertedValue := %s(rawValue)\n",
			renderedType(field.Type, aliases),
		)
		if field.Kind == runtimeconfig.KindInteger {
			source.WriteString("\t\tif int64(convertedValue) != rawValue {\n")
			fmt.Fprintf(
				source,
				"\t\t\treturn %s{}, %s.Errorf(%s)\n",
				outputType,
				fmtAlias,
				strconv.Quote(
					"decode configuration property "+field.Key+" for "+
						configType.TypeID+"."+field.Name+": value is outside "+field.TypeID,
				),
			)
			source.WriteString("\t\t}\n")
		}
		fmt.Fprintf(source, "\t\tvalue.%s = convertedValue\n", field.Name)
		source.WriteString("\t}\n")
	}
	source.WriteString("\treturn value, nil\n")
	source.WriteString("}\n\n")
}

func generatedConfigurationFunction(configType configuration.Type) string {
	digest := sha256.Sum256([]byte(configType.SymbolID))
	name := exportedGeneratedIdentifier(configType.Name, "Configuration")
	return "Bind" + name + "_" + hex.EncodeToString(digest[:4])
}

func configurationAccessor(kind runtimeconfig.Kind) string {
	switch kind {
	case runtimeconfig.KindString:
		return "RequiredString"
	case runtimeconfig.KindBoolean:
		return "Boolean"
	case runtimeconfig.KindInteger:
		return "Integer"
	case runtimeconfig.KindDuration:
		return "Duration"
	default:
		return "RequiredString"
	}
}

func configurationProviderIndex(configTypes []configuration.Type) map[string]configuration.Type {
	result := make(map[string]configuration.Type, len(configTypes))
	for _, configType := range configTypes {
		result[configType.SymbolID] = configType
	}
	return result
}

func renderedType(value types.Type, aliases map[string]string) string {
	return types.TypeString(value, func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		if alias, ok := aliases[pkg.Path()]; ok {
			return alias
		}
		return pkg.Name()
	})
}

func writeImports(source *bytes.Buffer, aliases map[string]string) {
	if len(aliases) == 0 {
		return
	}
	var standardPaths, applicationPaths []string
	for importPath := range aliases {
		if isStandardImport(importPath) {
			standardPaths = append(standardPaths, importPath)
		} else {
			applicationPaths = append(applicationPaths, importPath)
		}
	}
	sort.Strings(standardPaths)
	sort.Strings(applicationPaths)
	source.WriteString("import (\n")
	writeImportGroup(source, aliases, standardPaths)
	if len(standardPaths) != 0 && len(applicationPaths) != 0 {
		source.WriteByte('\n')
	}
	writeImportGroup(source, aliases, applicationPaths)
	source.WriteString(")\n\n")
}

func isStandardImport(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

func writeImportGroup(source *bytes.Buffer, aliases map[string]string, paths []string) {
	for _, importPath := range paths {
		fmt.Fprintf(source, "\t%s %s\n", aliases[importPath], strconv.Quote(importPath))
	}
}
