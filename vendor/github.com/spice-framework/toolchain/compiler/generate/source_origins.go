package generate

import (
	"bytes"
	"go/token"
	"path/filepath"
	"slices"
	"sort"

	"github.com/spice-framework/toolchain/compiler/application"
	compilerasync "github.com/spice-framework/toolchain/compiler/async"
	compilercache "github.com/spice-framework/toolchain/compiler/cache"
	"github.com/spice-framework/toolchain/compiler/configuration"
	"github.com/spice-framework/toolchain/compiler/controller"
	compilerevent "github.com/spice-framework/toolchain/compiler/event"
	compilerlifecycle "github.com/spice-framework/toolchain/compiler/lifecycle"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/modulith"
	compilerpolicy "github.com/spice-framework/toolchain/compiler/policy"
	"github.com/spice-framework/toolchain/compiler/provider"
	compilerschedule "github.com/spice-framework/toolchain/compiler/schedule"
	compilertransaction "github.com/spice-framework/toolchain/compiler/transaction"
)

func applicationSourceOrigins(
	program *load.Program,
	target application.Target,
) []SourceOrigin {
	if program == nil || target.PhysicalPosition.Filename == "" {
		return nil
	}
	pkg, found := packageByPath(program.Packages(), target.PackagePath)
	if !found || pkg.Raw == nil || pkg.Raw.Module == nil {
		return nil
	}
	relative, err := filepath.Rel(
		pkg.Raw.Module.Dir,
		target.PhysicalPosition.Filename,
	)
	if err != nil || !filepath.IsLocal(relative) {
		return nil
	}
	origin := SourceOrigin{
		Path:   filepath.ToSlash(relative),
		Line:   target.PhysicalPosition.Line,
		Column: target.PhysicalPosition.Column,
		Symbol: target.SymbolID,
	}
	if origin.Line == 0 {
		origin.Line = target.Position.Line
	}
	if origin.Column == 0 {
		origin.Column = target.Position.Column
	}
	return []SourceOrigin{origin}
}

func modelSourceOrigins(
	program *load.Program,
	model application.Model,
	applicationTarget application.Target,
	target Target,
) []SourceOrigin {
	collector := sourceOriginCollector{
		target:  target,
		origins: applicationSourceOrigins(program, applicationTarget),
	}
	collector.addProviders(model.Providers())
	collector.addConfigurations(model.Configurations())
	collector.addControllers(model.Controllers())
	collector.addLifecycle(model.Components())
	collector.addSchedules(model.Jobs())
	collector.addAsync(model.AsyncTasks())
	collector.addEvents(model.Events())
	collector.addBoundaries(model.Transactions(), model.Caches())
	collector.addPolicies(model.Policies())
	collector.addModules(model.Modules())
	sortSourceOrigins(collector.origins)
	return slices.Compact(collector.origins)
}

func (collector *sourceOriginCollector) addPolicies(
	services []compilerpolicy.Service,
) {
	for _, service := range services {
		for _, method := range service.Methods() {
			if !method.Decorated() {
				continue
			}
			collector.add(
				method.PhysicalPosition,
				method.Position,
				method.MethodID+"#policy",
				service.Provider.PackagePath,
			)
		}
	}
}

type sourceOriginCollector struct {
	target  Target
	origins []SourceOrigin
}

func (collector *sourceOriginCollector) add(
	position token.Position,
	display token.Position,
	symbolID string,
	packagePath string,
) {
	origin, ok := sourceOriginAt(
		position,
		display,
		symbolID,
		packagePath,
		collector.target,
	)
	if ok {
		collector.origins = append(collector.origins, origin)
	}
}

func (collector *sourceOriginCollector) addProviders(
	providers []provider.Provider,
) {
	for _, item := range providers {
		collector.add(
			item.PhysicalPosition,
			item.Position,
			item.SymbolID,
			item.PackagePath,
		)
	}
}

func (collector *sourceOriginCollector) addConfigurations(
	configTypes []configuration.Type,
) {
	for _, item := range configTypes {
		collector.add(
			item.PhysicalPosition,
			item.Position,
			item.SymbolID,
			item.PackagePath,
		)
		for _, field := range item.Fields() {
			collector.add(
				field.PhysicalPosition,
				field.Position,
				item.SymbolID+"."+field.Name,
				item.PackagePath,
			)
		}
	}
}

func (collector *sourceOriginCollector) addControllers(
	controllers []controller.Controller,
) {
	for _, item := range controllers {
		collector.add(
			item.PhysicalPosition,
			item.Position,
			item.SymbolID,
			item.PackagePath,
		)
		for _, route := range item.Routes() {
			collector.add(
				route.PhysicalPosition,
				route.Position,
				route.SymbolID,
				route.Symbol.PackagePath,
			)
			for _, binding := range route.Bindings() {
				collector.add(
					binding.PhysicalPosition,
					binding.Position,
					route.SymbolID+"."+binding.Field,
					route.Symbol.PackagePath,
				)
			}
			if authorization, ok := route.Authorization(); ok {
				collector.add(
					authorization.PhysicalPosition,
					authorization.Position,
					route.SymbolID+"#authorization",
					route.Symbol.PackagePath,
				)
			}
		}
	}
}

func (collector *sourceOriginCollector) addLifecycle(
	components []compilerlifecycle.Component,
) {
	for _, component := range components {
		for _, hook := range []*compilerlifecycle.Hook{
			component.Start,
			component.Stop,
		} {
			if hook == nil {
				continue
			}
			collector.add(
				hook.PhysicalPosition,
				hook.Position,
				hook.MethodID,
				hook.Method.PackagePath,
			)
		}
	}
}

func (collector *sourceOriginCollector) addSchedules(
	jobs []compilerschedule.Job,
) {
	for _, job := range jobs {
		collector.add(
			job.PhysicalPosition,
			job.Position,
			job.MethodID,
			job.Method.PackagePath,
		)
	}
}

func (collector *sourceOriginCollector) addAsync(
	tasks []compilerasync.Task,
) {
	for _, task := range tasks {
		collector.add(
			task.PhysicalPosition,
			task.Position,
			task.MethodID,
			task.Method.PackagePath,
		)
	}
}

func (collector *sourceOriginCollector) addEvents(
	topics []compilerevent.Topic,
) {
	for _, topic := range topics {
		collector.add(
			topic.PhysicalPosition,
			topic.Position,
			topic.MarkerID,
			topic.Marker.PackagePath,
		)
		for _, listener := range topic.Listeners() {
			collector.add(
				listener.PhysicalPosition,
				listener.Position,
				listener.MethodID,
				listener.Method.PackagePath,
			)
		}
	}
}

func (collector *sourceOriginCollector) addBoundaries(
	transactions []compilertransaction.Boundary,
	caches []compilercache.Boundary,
) {
	for _, boundary := range transactions {
		collector.add(
			boundary.PhysicalPosition,
			boundary.Position,
			boundary.RouteID+"#transaction",
			"",
		)
	}
	for _, boundary := range caches {
		collector.add(
			boundary.PhysicalPosition,
			boundary.Position,
			boundary.RouteID+"#cache",
			"",
		)
	}
}

func (collector *sourceOriginCollector) addModules(
	modules []modulith.Module,
) {
	for _, module := range modules {
		collector.add(
			module.PhysicalPosition,
			module.Position,
			module.ID+"#module",
			module.RootPackage,
		)
	}
}

func sourceOriginAt(
	physical token.Position,
	display token.Position,
	symbolID string,
	packagePath string,
	target Target,
) (SourceOrigin, bool) {
	sourceFile := physical.Filename
	if sourceFile == "" {
		sourceFile = display.Filename
	}
	if sourceFile == "" {
		return SourceOrigin{}, false
	}
	relative, err := filepath.Rel(target.ModuleRoot, sourceFile)
	var sourcePath string
	switch {
	case err == nil && filepath.IsLocal(relative):
		sourcePath = filepath.ToSlash(relative)
	case packagePath != "":
		sourcePath = packagePath + "/" + filepath.Base(sourceFile)
	default:
		return SourceOrigin{}, false
	}
	line := physical.Line
	if line == 0 {
		line = display.Line
	}
	column := physical.Column
	if column == 0 {
		column = display.Column
	}
	return SourceOrigin{
		Path:   sourcePath,
		Line:   line,
		Column: column,
		Symbol: symbolID,
	}, true
}

func sortSourceOrigins(origins []SourceOrigin) {
	sort.SliceStable(origins, func(left, right int) bool {
		if origins[left].Path != origins[right].Path {
			return origins[left].Path < origins[right].Path
		}
		if origins[left].Line != origins[right].Line {
			return origins[left].Line < origins[right].Line
		}
		if origins[left].Column != origins[right].Column {
			return origins[left].Column < origins[right].Column
		}
		return origins[left].Symbol < origins[right].Symbol
	})
}

func firstSourceOrigin(origins []SourceOrigin) *SourceOrigin {
	if len(origins) == 0 {
		return nil
	}
	result := origins[0]
	return &result
}

func sourceOriginForSymbol(
	origins []SourceOrigin,
	symbolID string,
) (SourceOrigin, bool) {
	for _, origin := range origins {
		if origin.Symbol == symbolID {
			return origin, true
		}
	}
	return SourceOrigin{}, false
}

func generatedIdentifierPosition(
	content []byte,
	identifier string,
) (int, int, bool) {
	offset := bytes.Index(content, []byte(identifier))
	if offset < 0 {
		return 0, 0, false
	}
	line := bytes.Count(content[:offset], []byte{'\n'}) + 1
	lineStart := bytes.LastIndexByte(content[:offset], '\n') + 1
	return line, offset - lineStart + 1, true
}
