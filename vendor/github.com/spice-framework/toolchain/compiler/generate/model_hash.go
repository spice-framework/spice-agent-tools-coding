package generate

import (
	"database/sql"
	"encoding/json"

	"github.com/spice-framework/spice/annotation/sdk"
	runtimeconfig "github.com/spice-framework/spice/config"
	"github.com/spice-framework/toolchain/compiler/application"
	compilerasync "github.com/spice-framework/toolchain/compiler/async"
	compilercache "github.com/spice-framework/toolchain/compiler/cache"
	"github.com/spice-framework/toolchain/compiler/controller"
	compilerevent "github.com/spice-framework/toolchain/compiler/event"
	"github.com/spice-framework/toolchain/compiler/provider"
	compilerschedule "github.com/spice-framework/toolchain/compiler/schedule"
	compilertransaction "github.com/spice-framework/toolchain/compiler/transaction"
)

type modelHashScheduleJob struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	Module          string `json:"module"`
	InitialDelay    int64  `json:"initial_delay"`
	Delay           int64  `json:"delay"`
	ContinueOnError bool   `json:"continue_on_error"`
}

type modelHashAsyncParameter struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
}

type modelHashAsyncTask struct {
	ID           string                    `json:"id"`
	Provider     string                    `json:"provider"`
	Module       string                    `json:"module"`
	SubmitMethod string                    `json:"submit_method"`
	Parameters   []modelHashAsyncParameter `json:"parameters"`
}

type modelHashEventListener struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Module   string `json:"module"`
	Order    int    `json:"order"`
}

type modelHashEventTopic struct {
	ID        string                   `json:"id"`
	Provider  string                   `json:"provider"`
	Module    string                   `json:"module"`
	Publisher string                   `json:"publisher"`
	Payload   string                   `json:"payload"`
	Listeners []modelHashEventListener `json:"listeners"`
}

type modelHashCache struct {
	Route  string `json:"route"`
	Name   string `json:"name"`
	Module string `json:"module"`
	Key    string `json:"key"`
	Value  string `json:"value"`
}

type modelHashDependency struct {
	Index      int                     `json:"index"`
	Type       string                  `json:"type"`
	Kind       provider.DependencyKind `json:"kind,omitempty"`
	Element    string                  `json:"element,omitempty"`
	Qualifiers []string                `json:"qualifiers,omitempty"`
}

type modelHashProvider struct {
	ID            string                `json:"id"`
	Source        provider.Source       `json:"source"`
	SourceID      string                `json:"source_id,omitempty"`
	SourceVersion string                `json:"source_version,omitempty"`
	Module        string                `json:"module,omitempty"`
	Output        string                `json:"output"`
	Construction  provider.Construction `json:"construction,omitempty"`
	Constructor   string                `json:"constructor,omitempty"`
	Interfaces    []string              `json:"interfaces,omitempty"`
	Name          string                `json:"name"`
	ExplicitName  bool                  `json:"explicit_name,omitempty"`
	Aliases       []string              `json:"aliases,omitempty"`
	Qualifiers    []string              `json:"qualifiers,omitempty"`
	Primary       bool                  `json:"primary,omitempty"`
	Fallback      bool                  `json:"fallback,omitempty"`
	Order         int64                 `json:"order,omitempty"`
	Scope         sdk.BeanScope         `json:"scope"`
	Cleanup       bool                  `json:"cleanup"`
	Error         bool                  `json:"error"`
	Inputs        []modelHashDependency `json:"inputs"`
}

type modelHashEdge struct {
	Consumer   string                  `json:"consumer"`
	Parameter  int                     `json:"parameter"`
	Provider   string                  `json:"provider"`
	Kind       provider.DependencyKind `json:"kind,omitempty"`
	Collection int                     `json:"collection,omitempty"`
}

type modelHashComponent struct {
	Provider string `json:"provider"`
	Start    string `json:"start"`
	Stop     string `json:"stop,omitempty"`
}

type modelHashRoot struct {
	Index    int    `json:"index"`
	Type     string `json:"type"`
	Provider string `json:"provider"`
}

type modelHashConfigurationField struct {
	Index       int                `json:"index"`
	Name        string             `json:"name"`
	Type        string             `json:"type"`
	Key         string             `json:"key"`
	Kind        runtimeconfig.Kind `json:"kind"`
	Module      string             `json:"module,omitempty"`
	Environment string             `json:"environment,omitempty"`
	Default     string             `json:"default,omitempty"`
	HasDefault  bool               `json:"has_default,omitempty"`
	Required    bool               `json:"required,omitempty"`
	Secret      bool               `json:"secret,omitempty"`
}

type modelHashConfiguration struct {
	ID     string                        `json:"id"`
	Type   string                        `json:"type"`
	Prefix string                        `json:"prefix,omitempty"`
	Module string                        `json:"module,omitempty"`
	Fields []modelHashConfigurationField `json:"fields"`
}

type modelHashBinding struct {
	Index    int                   `json:"index"`
	Field    string                `json:"field"`
	Name     string                `json:"name,omitempty"`
	Location controller.Location   `json:"location"`
	Required bool                  `json:"required"`
	Kind     controller.ScalarKind `json:"kind,omitempty"`
	Type     string                `json:"type"`
}

type modelHashAuthorization struct {
	PolicyID      string   `json:"policy_id"`
	Module        string   `json:"module"`
	Authenticated bool     `json:"authenticated,omitempty"`
	AnyRoles      []string `json:"any_roles,omitempty"`
	AllRoles      []string `json:"all_roles,omitempty"`
	AllScopes     []string `json:"all_scopes,omitempty"`
	Expression    string   `json:"expression,omitempty"`
}

type modelHashRoute struct {
	ID                string                  `json:"id"`
	Method            string                  `json:"method"`
	Path              string                  `json:"path"`
	Provider          string                  `json:"provider"`
	Request           string                  `json:"request,omitempty"`
	Response          string                  `json:"response,omitempty"`
	Validator         string                  `json:"validator,omitempty"`
	Raw               bool                    `json:"raw,omitempty"`
	NoContent         bool                    `json:"no_content,omitempty"`
	ExecutorParameter bool                    `json:"executor_parameter,omitempty"`
	Bindings          []modelHashBinding      `json:"bindings"`
	Authorization     *modelHashAuthorization `json:"authorization,omitempty"`
}

type modelHashController struct {
	ID       string           `json:"id"`
	Module   string           `json:"module,omitempty"`
	Provider string           `json:"provider"`
	Prefix   string           `json:"prefix,omitempty"`
	Routes   []modelHashRoute `json:"routes"`
}

type modelHashTransaction struct {
	Route     string             `json:"route"`
	Manager   string             `json:"manager"`
	Module    string             `json:"module"`
	Isolation sql.IsolationLevel `json:"isolation"`
	ReadOnly  bool               `json:"read_only,omitempty"`
}

type modelHashNamedInterface struct {
	Name        string `json:"name"`
	PackagePath string `json:"package"`
}

type modelHashModule struct {
	ID                  string                    `json:"id"`
	RootPackage         string                    `json:"root_package"`
	Packages            []string                  `json:"packages"`
	NamedInterfaces     []modelHashNamedInterface `json:"named_interfaces"`
	AllowedDependencies []string                  `json:"allowed_dependencies"`
}

type modelHashModuleEdge struct {
	FromModule  string `json:"from_module"`
	ToModule    string `json:"to_module"`
	API         string `json:"api"`
	FromPackage string `json:"from_package"`
	ToPackage   string `json:"to_package"`
}

type modelHashInput struct {
	Schema         int                         `json:"schema"`
	Target         TargetSummary               `json:"target"`
	Symbol         string                      `json:"symbol"`
	Providers      []modelHashProvider         `json:"providers"`
	Configurations []modelHashConfiguration    `json:"configurations"`
	Controllers    []modelHashController       `json:"controllers"`
	Transactions   []modelHashTransaction      `json:"transactions,omitempty"`
	Edges          []modelHashEdge             `json:"edges"`
	Components     []modelHashComponent        `json:"components"`
	Jobs           []modelHashScheduleJob      `json:"jobs"`
	AsyncTasks     []modelHashAsyncTask        `json:"async_tasks,omitempty"`
	Events         []modelHashEventTopic       `json:"events,omitempty"`
	Caches         []modelHashCache            `json:"caches,omitempty"`
	Roots          []modelHashRoot             `json:"roots"`
	Bootstrap      []modelHashBootstrapFeature `json:"bootstrap"`
	Modules        []modelHashModule           `json:"modules,omitempty"`
	ModuleEdges    []modelHashModuleEdge       `json:"module_edges,omitempty"`
	Unassigned     []string                    `json:"unassigned_packages,omitempty"`
}

func modelHash(
	model application.Model,
	applicationTarget application.Target,
	target Target,
) (string, error) {
	value := modelHashInput{
		Schema:    SchemaVersion,
		Target:    summarizeTarget(target),
		Symbol:    applicationTarget.SymbolID,
		Bootstrap: bootstrapHashInput(applicationTarget),
	}
	providers := model.Providers()
	providerModules := providerModuleIDs(model, providers)
	for _, item := range providers {
		inputs := make([]modelHashDependency, len(item.Dependencies))
		for index, dependency := range item.Dependencies {
			inputs[index] = modelHashDependency{
				Index:   dependency.Index,
				Type:    dependency.TypeID,
				Kind:    dependency.Kind,
				Element: dependency.ElementTypeID,
				Qualifiers: append(
					[]string(nil),
					dependency.Qualifiers...,
				),
			}
		}
		value.Providers = append(value.Providers, modelHashProvider{
			ID:            item.SymbolID,
			Source:        item.Source,
			SourceID:      item.SourceID,
			SourceVersion: item.SourceVersion,
			Module:        providerModules[item.SymbolID],
			Output:        item.OutputTypeID,
			Construction:  item.Construction,
			Constructor:   item.Constructor.ID,
			Name:          item.Name,
			ExplicitName:  item.ExplicitName,
			Aliases:       append([]string(nil), item.Aliases...),
			Qualifiers:    append([]string(nil), item.Qualifiers...),
			Primary:       item.Primary,
			Fallback:      item.Fallback,
			Order:         item.Order,
			Scope:         item.Scope,
			Cleanup:       item.ReturnsCleanup,
			Error:         item.ReturnsError,
			Inputs:        inputs,
		})
		for _, binding := range item.Interfaces {
			value.Providers[len(value.Providers)-1].Interfaces = append(
				value.Providers[len(value.Providers)-1].Interfaces,
				binding.TypeID,
			)
		}
	}
	for _, configType := range model.Configurations() {
		item := modelHashConfiguration{
			ID:     configType.SymbolID,
			Type:   configType.TypeID,
			Prefix: configType.Prefix,
			Module: configType.Module,
		}
		for _, field := range configType.Fields() {
			item.Fields = append(item.Fields, modelHashConfigurationField{
				Index:       field.Index,
				Name:        field.Name,
				Type:        field.TypeID,
				Key:         field.Key,
				Kind:        field.Kind,
				Module:      field.Module,
				Environment: field.Environment,
				Default:     field.Default,
				HasDefault:  field.HasDefault,
				Required:    field.Required,
				Secret:      field.Secret,
			})
		}
		value.Configurations = append(value.Configurations, item)
	}
	for _, item := range model.Controllers() {
		inputItem := modelHashController{
			ID:       item.SymbolID,
			Module:   item.Module,
			Provider: item.ProviderID,
			Prefix:   item.Prefix,
		}
		for _, route := range item.Routes() {
			routeInput := modelHashRoute{
				ID:                route.SymbolID,
				Method:            route.HTTPMethod,
				Path:              route.Path,
				Provider:          route.ProviderID,
				Request:           route.RequestTypeID,
				Response:          route.ResponseTypeID,
				Validator:         route.ValidatorID,
				Raw:               route.Raw,
				NoContent:         route.NoContent,
				ExecutorParameter: route.ExecutorParameter,
			}
			if authorization, protected := route.Authorization(); protected {
				routeInput.Authorization = &modelHashAuthorization{
					PolicyID:      authorization.PolicyID,
					Module:        authorization.Module,
					Authenticated: authorization.Authenticated,
					AnyRoles:      authorization.AnyRoles(),
					AllRoles:      authorization.AllRoles(),
					AllScopes:     authorization.AllScopes(),
					Expression:    authorization.Expression(),
				}
			}
			for _, binding := range route.Bindings() {
				routeInput.Bindings = append(routeInput.Bindings, modelHashBinding{
					Index:    binding.Index,
					Field:    binding.Field,
					Name:     binding.Name,
					Location: binding.Location,
					Required: binding.Required,
					Kind:     binding.Kind,
					Type:     binding.TypeID,
				})
			}
			inputItem.Routes = append(inputItem.Routes, routeInput)
		}
		value.Controllers = append(value.Controllers, inputItem)
	}
	value.Transactions = modelHashTransactions(model.Transactions())
	for _, edge := range model.Edges() {
		value.Edges = append(value.Edges, modelHashEdge{
			Consumer:   edge.ConsumerID,
			Parameter:  edge.ParameterIndex,
			Provider:   edge.DependencyID,
			Kind:       edge.DependencyKind,
			Collection: edge.CollectionIndex,
		})
	}
	for _, component := range model.Components() {
		item := modelHashComponent{
			Provider: component.Provider.SymbolID,
			Start:    component.Start.MethodID,
		}
		if component.Stop != nil {
			item.Stop = component.Stop.MethodID
		}
		value.Components = append(value.Components, item)
	}
	value.Jobs = modelHashScheduleJobs(model.Jobs(), providerModules)
	value.AsyncTasks = modelHashAsyncTasks(
		model.AsyncTasks(),
		providerModules,
	)
	value.Events = modelHashEvents(model.Events())
	value.Caches = modelHashCaches(model.Caches())
	addModelHashModules(&value, model, applicationTarget)
	for _, root := range applicationTarget.Roots() {
		value.Roots = append(value.Roots, modelHashRoot{
			Index:    root.Index,
			Type:     root.TypeID,
			Provider: root.ProviderID,
		})
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return contentHash(encoded), nil
}

func addModelHashModules(
	value *modelHashInput,
	model application.Model,
	applicationTarget application.Target,
) {
	if !commandFeaturesFor(
		applicationTarget,
		len(model.Controllers()) != 0,
	).modules {
		return
	}
	value.Modules = modelHashModules(model)
	value.ModuleEdges = modelHashModuleEdges(model)
	for _, item := range model.UnassignedPackages() {
		value.Unassigned = append(value.Unassigned, item.Path)
	}
}

func modelHashModules(model application.Model) []modelHashModule {
	modules := model.Modules()
	result := make([]modelHashModule, len(modules))
	for index, module := range modules {
		item := modelHashModule{
			ID:          module.ID,
			RootPackage: module.RootPackage,
		}
		for _, pkg := range module.Packages() {
			item.Packages = append(item.Packages, pkg.Path)
		}
		for _, namedInterface := range module.NamedInterfaces() {
			item.NamedInterfaces = append(
				item.NamedInterfaces,
				modelHashNamedInterface{
					Name:        namedInterface.Name,
					PackagePath: namedInterface.PackagePath,
				},
			)
		}
		for _, dependency := range module.AllowedDependencies() {
			item.AllowedDependencies = append(
				item.AllowedDependencies,
				dependency.String(),
			)
		}
		result[index] = item
	}
	return result
}

func modelHashModuleEdges(model application.Model) []modelHashModuleEdge {
	edges := model.ModuleEdges()
	result := make([]modelHashModuleEdge, len(edges))
	for index, edge := range edges {
		api := "default"
		if edge.API != "" {
			api = edge.API
		}
		result[index] = modelHashModuleEdge{
			FromModule:  edge.FromModule,
			ToModule:    edge.ToModule,
			API:         api,
			FromPackage: edge.FromPackage,
			ToPackage:   edge.ToPackage,
		}
	}
	return result
}

func modelHashEvents(topics []compilerevent.Topic) []modelHashEventTopic {
	result := make([]modelHashEventTopic, len(topics))
	for index, topic := range topics {
		item := modelHashEventTopic{
			ID:        topic.MarkerID,
			Provider:  topic.ProviderID,
			Module:    topic.Module,
			Publisher: topic.PublisherTypeID,
			Payload:   topic.PayloadTypeID,
		}
		for _, listener := range topic.Listeners() {
			item.Listeners = append(item.Listeners, modelHashEventListener{
				ID:       listener.MethodID,
				Provider: listener.ProviderID,
				Module:   listener.Module,
				Order:    listener.Order,
			})
		}
		result[index] = item
	}
	return result
}

func modelHashCaches(boundaries []compilercache.Boundary) []modelHashCache {
	result := make([]modelHashCache, len(boundaries))
	for index, boundary := range boundaries {
		result[index] = modelHashCache{
			Route:  boundary.RouteID,
			Name:   boundary.CacheName,
			Module: boundary.Module,
			Key:    boundary.KeyTypeID,
			Value:  boundary.ValueTypeID,
		}
	}
	return result
}

func modelHashTransactions(
	boundaries []compilertransaction.Boundary,
) []modelHashTransaction {
	result := make([]modelHashTransaction, len(boundaries))
	for index, boundary := range boundaries {
		result[index] = modelHashTransaction{
			Route:     boundary.RouteID,
			Manager:   boundary.ManagerProviderID,
			Module:    boundary.Module,
			Isolation: boundary.Isolation,
			ReadOnly:  boundary.ReadOnly,
		}
	}
	return result
}

func modelHashScheduleJobs(
	jobs []compilerschedule.Job,
	providerModules map[string]string,
) []modelHashScheduleJob {
	result := make([]modelHashScheduleJob, len(jobs))
	for index, job := range jobs {
		module := providerModules[job.ProviderID]
		if module == "" {
			module = job.Method.PackagePath
		}
		result[index] = modelHashScheduleJob{
			ID:              job.MethodID,
			Provider:        job.ProviderID,
			Module:          module,
			InitialDelay:    int64(job.InitialDelay),
			Delay:           int64(job.Delay),
			ContinueOnError: job.ContinueOnError,
		}
	}
	return result
}

func modelHashAsyncTasks(
	tasks []compilerasync.Task,
	providerModules map[string]string,
) []modelHashAsyncTask {
	result := make([]modelHashAsyncTask, len(tasks))
	for index, task := range tasks {
		item := modelHashAsyncTask{
			ID:           task.MethodID,
			Provider:     task.ProviderID,
			Module:       asyncTaskModule(task, providerModules),
			SubmitMethod: task.SubmitMethod,
		}
		for _, parameter := range task.Parameters() {
			item.Parameters = append(
				item.Parameters,
				modelHashAsyncParameter{
					Index: parameter.Index,
					Type:  parameter.TypeID,
				},
			)
		}
		result[index] = item
	}
	return result
}
