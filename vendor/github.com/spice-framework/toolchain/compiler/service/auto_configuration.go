package service

import (
	"sort"

	compilerauto "github.com/spice-framework/toolchain/compiler/autoconfigure"
	"github.com/spice-framework/toolchain/compiler/diagnostic"
	diagnosticadapt "github.com/spice-framework/toolchain/compiler/diagnostic/adapt"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

func (service *Service) prepareProviderCatalogs(
	request normalizedRequest,
	program *load.Program,
	resolution resolve.Result,
	primary provider.Catalog,
	discovery compilerauto.Discovery,
) ([]provider.Catalog, []AutoConfiguration, diagnostic.Set) {
	configurations, err := compilerauto.Decode(
		program,
		discovery.Selected(program),
	)
	if err != nil {
		return nil, nil, diagnosticadapt.Failure(
			"auto-configuration",
			"descriptor",
			err.Error(),
		)
	}
	catalogs, diagnostics := service.buildProviderCatalogs(
		request,
		program,
		resolution,
	)
	if !diagnostics.Empty() {
		return nil, nil, diagnostics
	}
	autoCatalog, decisions, diagnostics := buildAutoConfigurationCatalog(
		request,
		program,
		primary,
		configurations,
	)
	summary := summarizeAutoConfigurations(configurations, decisions)
	if !diagnostics.Empty() {
		return nil, summary, diagnostics
	}
	if len(autoCatalog.Providers()) != 0 {
		catalogs = append(catalogs, autoCatalog)
	}
	return catalogs, summary, diagnostic.NewSet()
}

func buildAutoConfigurationCatalog(
	request normalizedRequest,
	program *load.Program,
	primary provider.Catalog,
	configurations []compilerauto.Configuration,
) (
	provider.Catalog,
	[]provider.AutoConfigurationDecision,
	diagnostic.Set,
) {
	var entrypoints []provider.Entrypoint
	for _, configuration := range configurations {
		for _, bean := range configuration.Beans {
			entrypoints = append(entrypoints, provider.Entrypoint{
				PackagePath:   bean.PackagePath,
				Symbol:        bean.Factory,
				SourceID:      bean.SourceID,
				SourceVersion: bean.SourceVersion,
				Source:        provider.SourceAutoConfiguration,
				Name:          bean.Name,
				Aliases:       bean.Aliases,
				Qualifiers:    bean.Qualifiers,
				Primary:       bean.Primary,
				Fallback:      bean.Fallback,
				Order:         bean.Order,
			})
		}
	}
	if len(entrypoints) == 0 {
		return provider.Catalog{}, nil, diagnostic.NewSet()
	}
	defaults := provider.BuildEntrypoints(program, entrypoints)
	if diagnostics := defaults.Diagnostics(); len(diagnostics) != 0 {
		return provider.Catalog{}, nil, versionDiagnostics(
			diagnosticadapt.Provider(request.root, diagnostics),
			request.overlay,
		)
	}
	selected, decisions := provider.SelectAutoConfiguration(primary, defaults)
	if diagnostics := selected.Diagnostics(); len(diagnostics) != 0 {
		return provider.Catalog{}, decisions, versionDiagnostics(
			diagnosticadapt.Provider(request.root, diagnostics),
			request.overlay,
		)
	}
	return selected, decisions, diagnostic.NewSet()
}

func summarizeAutoConfigurations(
	configurations []compilerauto.Configuration,
	decisions []provider.AutoConfigurationDecision,
) []AutoConfiguration {
	metadata := make(map[string]compilerauto.Bean)
	for _, configuration := range configurations {
		for _, bean := range configuration.Beans {
			metadata[bean.PackagePath+"\x00"+bean.Factory] = bean
		}
	}
	result := make([]AutoConfiguration, 0, len(decisions))
	for _, decision := range decisions {
		item := decision.Provider
		bean := metadata[item.PackagePath+"\x00"+item.Constructor.Name]
		if bean.Factory == "" {
			bean = metadata[item.PackagePath+"\x00"+item.Name]
		}
		result = append(result, AutoConfiguration{
			PackagePath:     item.PackagePath,
			Factory:         item.Constructor.Name,
			OutputTypeID:    item.OutputTypeID,
			Status:          decision.Status,
			Reason:          decision.Reason,
			ModulePath:      bean.ModulePath,
			ModuleVersion:   bean.ModuleVersion,
			ReplacementPath: bean.ReplacementPath,
			Review:          bean.Review,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].PackagePath != result[j].PackagePath {
			return result[i].PackagePath < result[j].PackagePath
		}
		return result[i].Factory < result[j].Factory
	})
	return result
}
