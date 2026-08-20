package generate

import (
	"fmt"
	"go/token"

	"github.com/spice-framework/toolchain/compiler/application"
	compilerlifecycle "github.com/spice-framework/toolchain/compiler/lifecycle"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
)

func validateTarget(target Target, applicationTarget application.Target) []Diagnostic {
	var diagnostics []Diagnostic
	add := func(kind, message string) {
		diagnostics = append(diagnostics, targetDiagnostic(applicationTarget, kind, message))
	}
	if !targetIDPattern.MatchString(target.ID) {
		add("target-id", fmt.Sprintf("generation target ID %q must match %s", target.ID, targetIDPattern))
	}
	if target.ModulePath == "" {
		add("module-path", "generation target module path is required")
	}
	if target.ModuleRoot == "" {
		add("module-root", "generation target module root is required")
	}
	if target.PackagePath == "" {
		add("package-path", "generated package import path is required")
	}
	if target.OutputDir == "" {
		add("output-dir", "generated output directory is required")
	}
	diagnostics = append(
		diagnostics,
		validateTargetLayout(target, applicationTarget)...,
	)
	if target.ManifestPath == "" {
		add("manifest-path", "generated manifest path is required")
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateTargetLayout(
	target Target,
	applicationTarget application.Target,
) []Diagnostic {
	add := func(kind, message string) Diagnostic {
		return targetDiagnostic(applicationTarget, kind, message)
	}
	switch target.Layout {
	case LayoutApplicationPackage:
		var diagnostics []Diagnostic
		if target.BridgeDir == "" {
			diagnostics = append(diagnostics, add(
				"bridge-dir",
				"application-package entrypoint directory is required",
			))
		}
		if target.EntrypointPackagePath == "" {
			diagnostics = append(diagnostics, add(
				"entrypoint-package",
				"application-package entrypoint import path is required",
			))
		}
		return diagnostics
	case LayoutGeneratedPackage:
		var diagnostics []Diagnostic
		if target.BridgeDir != "" {
			diagnostics = append(diagnostics, add(
				"bridge-dir",
				"generated-package target must not declare an entrypoint directory",
			))
		}
		if target.EntrypointPackagePath != "" {
			diagnostics = append(diagnostics, add(
				"entrypoint-package",
				"generated-package target must not declare an entrypoint import path",
			))
		}
		return diagnostics
	default:
		return []Diagnostic{add(
			"layout",
			fmt.Sprintf(
				"generation target layout %q is unsupported",
				target.Layout,
			),
		)}
	}
}

func validateRenderable(
	program *load.Program,
	model application.Model,
	applicationTarget application.Target,
	target Target,
) []Diagnostic {
	var diagnostics []Diagnostic
	packages := program.Packages()
	diagnostics = append(
		diagnostics,
		generatedImportCycleDiagnostics(
			packages,
			applicationTarget,
			target,
		)...,
	)
	diagnostics = append(
		diagnostics,
		providerVisibilityDiagnostics(
			packages,
			model.Providers(),
			applicationTarget,
			target,
		)...,
	)
	diagnostics = append(
		diagnostics,
		lifecycleVisibilityDiagnostics(
			model.Components(),
			applicationTarget,
		)...,
	)
	diagnostics = append(
		diagnostics,
		scheduledMethodDiagnostics(model, applicationTarget)...,
	)
	diagnostics = append(
		diagnostics,
		reservedConfigurationDiagnostics(model, applicationTarget)...,
	)
	sortDiagnostics(diagnostics)
	return diagnostics
}

func generatedImportCycleDiagnostics(
	packages []load.Package,
	applicationTarget application.Target,
	target Target,
) []Diagnostic {
	if target.Layout != LayoutGeneratedPackage {
		return nil
	}
	sourcePackage, ok := packageByPath(
		packages,
		applicationTarget.PackagePath,
	)
	if !ok || sourcePackage.Types == nil {
		return nil
	}
	var diagnostics []Diagnostic
	for _, imported := range sourcePackage.Types.Imports() {
		if imported.Path() != target.PackagePath {
			continue
		}
		diagnostics = append(diagnostics, targetDiagnostic(
			applicationTarget,
			"import-cycle",
			fmt.Sprintf(
				"application package %s imports generated package %s; generated output must be imported only by an outer command package",
				sourcePackage.Path,
				target.PackagePath,
			),
		))
	}
	return diagnostics
}

func providerVisibilityDiagnostics(
	packages []load.Package,
	providers []provider.Provider,
	applicationTarget application.Target,
	target Target,
) []Diagnostic {
	packageNames := make(map[string]string, len(packages))
	for _, pkg := range packages {
		packageNames[pkg.Path] = pkg.Name
	}
	var diagnostics []Diagnostic
	for _, item := range providers {
		switch {
		case item.PackagePath == target.PackagePath:
			diagnostics = append(diagnostics, providerRenderDiagnostic(
				applicationTarget,
				item,
				"self-import",
				fmt.Sprintf("provider %s is declared in generated package %s", item.SymbolID, target.PackagePath),
			))
		case packageNames[item.PackagePath] == "main":
			diagnostics = append(diagnostics, providerRenderDiagnostic(
				applicationTarget,
				item,
				"main-package",
				fmt.Sprintf(
					"provider %s is declared in package main, which generated package %s cannot import; move providers into an importable package",
					item.SymbolID,
					target.PackagePath,
				),
			))
		case !providerConstructionExported(item):
			diagnostics = append(diagnostics, providerRenderDiagnostic(
				applicationTarget,
				item,
				"unexported-provider",
				fmt.Sprintf(
					"provider %s construction is unexported; target-scoped generated packages require exported @Bean functions or exported stereotype types and constructors",
					item.SymbolID,
				),
			))
		}
	}
	return diagnostics
}

func providerConstructionExported(item provider.Provider) bool {
	if item.Construction == provider.ConstructionAllocate {
		return token.IsExported(item.Symbol.Name)
	}
	if item.Constructor.Name != "" {
		return token.IsExported(item.Constructor.Name)
	}
	return token.IsExported(item.Name)
}

func lifecycleVisibilityDiagnostics(
	components []compilerlifecycle.Component,
	applicationTarget application.Target,
) []Diagnostic {
	var diagnostics []Diagnostic
	for _, component := range components {
		methods := []load.Symbol{component.Start.Method}
		if component.Stop != nil {
			methods = append(methods, component.Stop.Method)
		}
		for _, method := range methods {
			if token.IsExported(method.Name) {
				continue
			}
			diagnostics = append(diagnostics, Diagnostic{
				Position:         method.Position,
				PhysicalPosition: method.PhysicalPosition,
				TargetID:         applicationTarget.SymbolID,
				Kind:             "unexported-hook",
				Message: fmt.Sprintf(
					"lifecycle method %s is unexported; target-scoped generated packages require exported hook methods",
					method.ID,
				),
			})
		}
	}
	return diagnostics
}

func scheduledMethodDiagnostics(
	model application.Model,
	applicationTarget application.Target,
) []Diagnostic {
	var diagnostics []Diagnostic
	for _, job := range model.Jobs() {
		if token.IsExported(job.Method.Name) {
			continue
		}
		diagnostics = append(diagnostics, Diagnostic{
			Position:         job.Position,
			PhysicalPosition: job.PhysicalPosition,
			TargetID:         applicationTarget.SymbolID,
			Kind:             "unexported-scheduled-method",
			Message: fmt.Sprintf(
				"scheduled method %s is unexported; target-scoped generated packages require exported scheduled methods",
				job.MethodID,
			),
		})
	}
	return diagnostics
}

func reservedConfigurationDiagnostics(
	model application.Model,
	applicationTarget application.Target,
) []Diagnostic {
	var diagnostics []Diagnostic
	reservedKeys := map[string]struct{}{
		shutdownConfigurationKey: {},
	}
	reservedEnvironments := map[string]struct{}{
		"SPICE_SHUTDOWN_TIMEOUT": {},
	}
	if len(model.AsyncTasks()) != 0 {
		reservedKeys[asyncConcurrencyKey] = struct{}{}
		reservedEnvironments["SPICE_ASYNC_MAX_CONCURRENCY"] = struct{}{}
	}
	cacheBoundaries := append(
		model.Caches(),
		serviceCacheBoundaries(model.Policies())...,
	)
	for _, boundary := range cacheBoundaries {
		reservedKeys[cacheCapacityKey(boundary.CacheName)] = struct{}{}
		reservedKeys[cacheTTLKey(boundary.CacheName)] = struct{}{}
		reservedEnvironments[cacheEnvironment(
			boundary.CacheName,
			"CAPACITY",
		)] = struct{}{}
		reservedEnvironments[cacheEnvironment(
			boundary.CacheName,
			"TTL",
		)] = struct{}{}
	}
	for _, configType := range model.Configurations() {
		for _, field := range configType.Fields() {
			if _, reserved := reservedKeys[field.Key]; reserved {
				diagnostics = append(diagnostics, Diagnostic{
					Position:         field.Position,
					PhysicalPosition: field.PhysicalPosition,
					TargetID:         applicationTarget.SymbolID,
					Kind:             "reserved-configuration",
					Message: fmt.Sprintf(
						"configuration field %s.%s uses framework-owned key %q; choose a different prefix or field key",
						configType.TypeID,
						field.Name,
						field.Key,
					),
				})
				continue
			}
			if _, reserved := reservedEnvironments[field.Environment]; !reserved {
				continue
			}
			diagnostics = append(diagnostics, Diagnostic{
				Position:         field.Position,
				PhysicalPosition: field.PhysicalPosition,
				TargetID:         applicationTarget.SymbolID,
				Kind:             "reserved-configuration",
				Message: fmt.Sprintf(
					"configuration field %s.%s uses framework-owned environment variable %q; choose a different environment mapping",
					configType.TypeID,
					field.Name,
					field.Environment,
				),
			})
		}
	}
	return diagnostics
}
