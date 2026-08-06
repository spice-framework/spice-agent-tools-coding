package sdk

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// ContributionKind identifies one typed compiler-IR input returned by an
// annotation handler.
type ContributionKind string

const (
	ContributionApplication    ContributionKind = "application"
	ContributionStereotype     ContributionKind = "stereotype"
	ContributionInterface      ContributionKind = "interface-binding"
	ContributionProvider       ContributionKind = "provider"
	ContributionBeanMetadata   ContributionKind = "bean-metadata"
	ContributionConfiguration  ContributionKind = "configuration"
	ContributionController     ContributionKind = "controller"
	ContributionRoute          ContributionKind = "route"
	ContributionModule         ContributionKind = "module"
	ContributionNamedInterface ContributionKind = "named-interface"
	ContributionLifecycle      ContributionKind = "lifecycle"
	ContributionBootstrap      ContributionKind = "bootstrap"
	ContributionSchedule       ContributionKind = "schedule"
	ContributionAsync          ContributionKind = "async"
	ContributionTransaction    ContributionKind = "transaction"
	ContributionEventTopic     ContributionKind = "event-topic"
	ContributionEventListener  ContributionKind = "event-listener"
	ContributionCache          ContributionKind = "cache"
	ContributionAuthorization  ContributionKind = "authorization"
	ContributionGeneratedFile  ContributionKind = "generated-file"
)

// ApplicationContribution marks the invocation target as an application
// marker. The compiler derives roots from the target's exact Go signature.
type ApplicationContribution struct{}

// StereotypeContribution classifies a declaration for architecture and tooling
// without inventing construction behavior. Providers remain explicit.
type StereotypeContribution struct {
	Role        string   `json:"role"`
	Construct   bool     `json:"construct,omitempty"`
	Constructor string   `json:"constructor,omitempty"`
	Name        string   `json:"name,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

// InterfaceBindingContribution explicitly exposes one concrete bean through
// named Go interfaces. Expressions are resolved against the invocation's
// physical source file by the typed compiler; handlers never guess method
// assignability.
type InterfaceBindingContribution struct {
	Interfaces []string `json:"interfaces"`
}

// ProviderContribution marks the invocation target as a provider. The
// compiler derives inputs, output, cleanup, and error behavior from go/types.
type ProviderContribution struct {
	Name    string   `json:"name,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

// BeanScope identifies which generated owner controls a bean instance and its
// cleanup. Scope values are compiler inputs, not runtime string lookups.
type BeanScope string

const (
	BeanScopeSingleton BeanScope = "singleton"
	BeanScopePrototype BeanScope = "prototype"
	BeanScopeRequest   BeanScope = "request"
	BeanScopeSession   BeanScope = "session"
)

// BeanMetadataContribution adds selection and ownership metadata to a bean or
// one constructor parameter. Qualifiers on parameters are requests; all other
// fields describe the bean on the annotated declaration.
type BeanMetadataContribution struct {
	Qualifiers []string  `json:"qualifiers,omitempty"`
	Primary    bool      `json:"primary,omitempty"`
	Fallback   bool      `json:"fallback,omitempty"`
	Order      *int64    `json:"order,omitempty"`
	Scope      BeanScope `json:"scope,omitempty"`
}

// ConfigurationContribution marks one typed configuration declaration.
type ConfigurationContribution struct {
	Prefix string `json:"prefix,omitempty"`
}

// ControllerContribution marks one HTTP controller declaration.
type ControllerContribution struct {
	Prefix string `json:"prefix,omitempty"`
}

// RouteContribution describes one generated HTTP route.
type RouteContribution struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// ModuleContribution describes one application module root.
type ModuleContribution struct {
	AllowedDependencies []string `json:"allowed_dependencies,omitempty"`
}

// NamedInterfaceContribution exposes one named module API.
type NamedInterfaceContribution struct {
	Name string `json:"name"`
}

// LifecyclePhase identifies when a lifecycle callback runs.
type LifecyclePhase string

const (
	LifecycleStart LifecyclePhase = "start"
	LifecycleStop  LifecyclePhase = "stop"
)

// LifecycleContribution describes one start or stop callback.
type LifecycleContribution struct {
	Phase LifecyclePhase `json:"phase"`
}

// BootstrapContribution activates one application-platform capability with
// deterministic string metadata owned by the handler.
type BootstrapContribution struct {
	Capability string            `json:"capability"`
	Options    []BootstrapOption `json:"options,omitempty"`
}

// BootstrapOption is one named, typed capability setting.
type BootstrapOption struct {
	Name  string            `json:"name"`
	Value ContributionValue `json:"value"`
}

// ContributionValue is a recursively typed literal safe for SDK transport.
type ContributionValue struct {
	Kind       Kind                `json:"kind"`
	String     string              `json:"string,omitempty"`
	Integer    int64               `json:"integer,omitempty"`
	Boolean    bool                `json:"boolean,omitempty"`
	Identifier string              `json:"identifier,omitempty"`
	List       []ContributionValue `json:"list,omitempty"`
}

// ScheduleContribution describes one fixed-delay scheduled method.
type ScheduleContribution struct {
	Delay           string `json:"delay"`
	InitialDelay    string `json:"initial_delay,omitempty"`
	ContinueOnError bool   `json:"continue_on_error,omitempty"`
}

// AsyncContribution marks a method for generated asynchronous execution.
type AsyncContribution struct{}

// TransactionContribution describes one generated transaction boundary.
type TransactionContribution struct {
	Isolation string `json:"isolation,omitempty"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// EventTopicContribution marks a typed event topic provider.
type EventTopicContribution struct{}

// EventListenerContribution describes one ordered typed event listener.
type EventListenerContribution struct {
	Order int64 `json:"order,omitempty"`
}

// CacheContribution describes one named generated cache boundary.
type CacheContribution struct {
	Name string `json:"name"`
}

// AuthorizationContribution describes one secure-deny route policy.
type AuthorizationContribution struct {
	Authenticated bool     `json:"authenticated,omitempty"`
	AnyRoles      []string `json:"any_roles,omitempty"`
	AllRoles      []string `json:"all_roles,omitempty"`
	AllScopes     []string `json:"all_scopes,omitempty"`
	Expression    string   `json:"expression,omitempty"`
}

// GeneratedFileContribution requests one guarded generated file. The compiler
// still owns path safety, generated markers, ownership, and filesystem apply.
type GeneratedFileContribution struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Contribution is a discriminated typed SDK union. Exactly one payload
// matching Kind must be present.
type Contribution struct {
	Kind           ContributionKind
	Application    *ApplicationContribution
	Stereotype     *StereotypeContribution
	Interface      *InterfaceBindingContribution
	Provider       *ProviderContribution
	BeanMetadata   *BeanMetadataContribution
	Configuration  *ConfigurationContribution
	Controller     *ControllerContribution
	Route          *RouteContribution
	Module         *ModuleContribution
	NamedInterface *NamedInterfaceContribution
	Lifecycle      *LifecycleContribution
	Bootstrap      *BootstrapContribution
	Schedule       *ScheduleContribution
	Async          *AsyncContribution
	Transaction    *TransactionContribution
	EventTopic     *EventTopicContribution
	EventListener  *EventListenerContribution
	Cache          *CacheContribution
	Authorization  *AuthorizationContribution
	GeneratedFile  *GeneratedFileContribution
}

// Validate rejects malformed, ambiguous, or unknown contribution payloads.
func (contribution Contribution) Validate() error {
	if contribution.payloadCount() != 1 {
		return errors.New(
			"annotation contribution requires exactly one typed payload",
		)
	}
	for _, validate := range []func(Contribution) (error, bool){
		validateFoundationContribution,
		validateWebModuleContribution,
		validateExecutionContribution,
		validateIntegrationContribution,
	} {
		if err, found := validate(contribution); found {
			return err
		}
	}
	return fmt.Errorf(
		"annotation contribution kind %q is unsupported",
		contribution.Kind,
	)
}

func validateFoundationContribution(
	contribution Contribution,
) (error, bool) {
	if err, found := validateBeanContribution(contribution); found {
		return err, true
	}
	//nolint:exhaustive // Contribution cases are deliberately partitioned across validators.
	switch contribution.Kind {
	case ContributionApplication:
		return requirePayload(
			contribution.Application,
			"application",
		), true
	case ContributionConfiguration:
		if err := requirePayload(
			contribution.Configuration,
			"configuration",
		); err != nil {
			return err, true
		}
		return validateOptionalPrefix(
			"configuration",
			contribution.Configuration.Prefix,
		), true
	case ContributionController:
		if err := requirePayload(
			contribution.Controller,
			"controller",
		); err != nil {
			return err, true
		}
		return validateOptionalRoutePrefix(
			contribution.Controller.Prefix,
		), true
	default:
		return nil, false
	}
}

func validateBeanContribution(
	contribution Contribution,
) (error, bool) {
	//nolint:exhaustive // Bean contribution cases are deliberately isolated.
	switch contribution.Kind {
	case ContributionStereotype:
		return validateStereotypeContribution(contribution)
	case ContributionInterface:
		if err := requirePayload(
			contribution.Interface,
			"interface binding",
		); err != nil {
			return err, true
		}
		if len(contribution.Interface.Interfaces) == 0 {
			return errors.New(
				"annotation interface binding requires at least one interface",
			), true
		}
		return validateUniqueTrimmed(
			"interface expression",
			contribution.Interface.Interfaces,
		), true
	case ContributionProvider:
		if err := requirePayload(
			contribution.Provider,
			"provider",
		); err != nil {
			return err, true
		}
		return validateBeanIdentity(
			contribution.Provider.Name,
			contribution.Provider.Aliases,
		), true
	case ContributionBeanMetadata:
		if err := requirePayload(
			contribution.BeanMetadata,
			"bean metadata",
		); err != nil {
			return err, true
		}
		return validateBeanMetadata(*contribution.BeanMetadata), true
	default:
		return nil, false
	}
}

func validateStereotypeContribution(
	contribution Contribution,
) (error, bool) {
	if err := requirePayload(
		contribution.Stereotype,
		"stereotype",
	); err != nil {
		return err, true
	}
	if err := requireTrimmed(
		"stereotype role",
		contribution.Stereotype.Role,
	); err != nil {
		return err, true
	}
	if contribution.Stereotype.Constructor != "" {
		if err := requireTrimmed(
			"stereotype constructor",
			contribution.Stereotype.Constructor,
		); err != nil {
			return err, true
		}
	}
	return validateBeanIdentity(
		contribution.Stereotype.Name,
		contribution.Stereotype.Aliases,
	), true
}

func validateWebModuleContribution(
	contribution Contribution,
) (error, bool) {
	//nolint:exhaustive // Contribution cases are deliberately partitioned across validators.
	switch contribution.Kind {
	case ContributionRoute:
		if err := requirePayload(contribution.Route, "route"); err != nil {
			return err, true
		}
		return validateRoute(*contribution.Route), true
	case ContributionModule:
		if err := requirePayload(contribution.Module, "module"); err != nil {
			return err, true
		}
		return validateUniqueTrimmed(
			"module allowed dependency",
			contribution.Module.AllowedDependencies,
		), true
	case ContributionNamedInterface:
		if err := requirePayload(
			contribution.NamedInterface,
			"named interface",
		); err != nil {
			return err, true
		}
		return requireTrimmed(
			"named interface name",
			contribution.NamedInterface.Name,
		), true
	case ContributionLifecycle:
		if err := requirePayload(
			contribution.Lifecycle,
			"lifecycle",
		); err != nil {
			return err, true
		}
		if contribution.Lifecycle.Phase != LifecycleStart &&
			contribution.Lifecycle.Phase != LifecycleStop {
			return fmt.Errorf(
				"annotation lifecycle contribution has unsupported phase %q",
				contribution.Lifecycle.Phase,
			), true
		}
		return nil, true
	case ContributionBootstrap:
		if err := requirePayload(
			contribution.Bootstrap,
			"bootstrap",
		); err != nil {
			return err, true
		}
		return validateBootstrap(*contribution.Bootstrap), true
	default:
		return nil, false
	}
}

func validateExecutionContribution(
	contribution Contribution,
) (error, bool) {
	//nolint:exhaustive // Contribution cases are deliberately partitioned across validators.
	switch contribution.Kind {
	case ContributionSchedule:
		if err := requirePayload(
			contribution.Schedule,
			"schedule",
		); err != nil {
			return err, true
		}
		return requireTrimmed(
			"schedule delay",
			contribution.Schedule.Delay,
		), true
	case ContributionAsync:
		return requirePayload(contribution.Async, "async"), true
	case ContributionTransaction:
		return requirePayload(
			contribution.Transaction,
			"transaction",
		), true
	case ContributionEventTopic:
		return requirePayload(
			contribution.EventTopic,
			"event topic",
		), true
	case ContributionEventListener:
		return requirePayload(
			contribution.EventListener,
			"event listener",
		), true
	default:
		return nil, false
	}
}

func validateIntegrationContribution(
	contribution Contribution,
) (error, bool) {
	//nolint:exhaustive // Contribution cases are deliberately partitioned across validators.
	switch contribution.Kind {
	case ContributionCache:
		if err := requirePayload(contribution.Cache, "cache"); err != nil {
			return err, true
		}
		return requireTrimmed(
			"cache name",
			contribution.Cache.Name,
		), true
	case ContributionAuthorization:
		if err := requirePayload(
			contribution.Authorization,
			"authorization",
		); err != nil {
			return err, true
		}
		return validateAuthorization(*contribution.Authorization), true
	case ContributionGeneratedFile:
		if err := requirePayload(
			contribution.GeneratedFile,
			"generated file",
		); err != nil {
			return err, true
		}
		return validateGeneratedFile(*contribution.GeneratedFile), true
	default:
		return nil, false
	}
}

// Clone returns a deep defensive copy.
func (contribution Contribution) Clone() Contribution {
	result := contribution
	result.Application = clonePointer(contribution.Application)
	result.Stereotype = clonePointer(contribution.Stereotype)
	if result.Stereotype != nil {
		result.Stereotype.Aliases = slices.Clone(
			contribution.Stereotype.Aliases,
		)
	}
	result.Interface = clonePointer(contribution.Interface)
	if result.Interface != nil {
		result.Interface.Interfaces = slices.Clone(
			contribution.Interface.Interfaces,
		)
	}
	result.Provider = clonePointer(contribution.Provider)
	if result.Provider != nil {
		result.Provider.Aliases = slices.Clone(
			contribution.Provider.Aliases,
		)
	}
	result.BeanMetadata = clonePointer(contribution.BeanMetadata)
	if result.BeanMetadata != nil {
		result.BeanMetadata.Qualifiers = slices.Clone(
			contribution.BeanMetadata.Qualifiers,
		)
		result.BeanMetadata.Order = clonePointer(
			contribution.BeanMetadata.Order,
		)
	}
	result.Configuration = clonePointer(contribution.Configuration)
	result.Controller = clonePointer(contribution.Controller)
	result.Route = clonePointer(contribution.Route)
	result.Module = clonePointer(contribution.Module)
	if result.Module != nil {
		result.Module.AllowedDependencies = slices.Clone(
			contribution.Module.AllowedDependencies,
		)
	}
	result.NamedInterface = clonePointer(contribution.NamedInterface)
	result.Lifecycle = clonePointer(contribution.Lifecycle)
	result.Bootstrap = clonePointer(contribution.Bootstrap)
	if result.Bootstrap != nil {
		result.Bootstrap.Options = cloneBootstrapOptions(
			contribution.Bootstrap.Options,
		)
	}
	result.Schedule = clonePointer(contribution.Schedule)
	result.Async = clonePointer(contribution.Async)
	result.Transaction = clonePointer(contribution.Transaction)
	result.EventTopic = clonePointer(contribution.EventTopic)
	result.EventListener = clonePointer(contribution.EventListener)
	result.Cache = clonePointer(contribution.Cache)
	result.Authorization = clonePointer(contribution.Authorization)
	if result.Authorization != nil {
		result.Authorization.AnyRoles = slices.Clone(
			contribution.Authorization.AnyRoles,
		)
		result.Authorization.AllRoles = slices.Clone(
			contribution.Authorization.AllRoles,
		)
		result.Authorization.AllScopes = slices.Clone(
			contribution.Authorization.AllScopes,
		)
	}
	result.GeneratedFile = clonePointer(contribution.GeneratedFile)
	return result
}

func (contribution Contribution) payloadCount() int {
	count := 0
	for _, present := range []bool{
		contribution.Application != nil,
		contribution.Stereotype != nil,
		contribution.Interface != nil,
		contribution.Provider != nil,
		contribution.BeanMetadata != nil,
		contribution.Configuration != nil,
		contribution.Controller != nil,
		contribution.Route != nil,
		contribution.Module != nil,
		contribution.NamedInterface != nil,
		contribution.Lifecycle != nil,
		contribution.Bootstrap != nil,
		contribution.Schedule != nil,
		contribution.Async != nil,
		contribution.Transaction != nil,
		contribution.EventTopic != nil,
		contribution.EventListener != nil,
		contribution.Cache != nil,
		contribution.Authorization != nil,
		contribution.GeneratedFile != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

func validateBeanIdentity(name string, aliases []string) error {
	if name != "" {
		if err := requireTrimmed("bean name", name); err != nil {
			return err
		}
	}
	if err := validateUniqueTrimmed("bean alias", aliases); err != nil {
		return err
	}
	for _, alias := range aliases {
		if alias == name {
			return fmt.Errorf(
				"bean alias %q duplicates the bean name",
				alias,
			)
		}
	}
	return nil
}

func validateBeanMetadata(metadata BeanMetadataContribution) error {
	if err := validateUniqueTrimmed(
		"bean qualifier",
		metadata.Qualifiers,
	); err != nil {
		return err
	}
	if metadata.Primary && metadata.Fallback {
		return errors.New(
			"bean metadata cannot be both primary and fallback",
		)
	}
	switch metadata.Scope {
	case "", BeanScopeSingleton, BeanScopePrototype,
		BeanScopeRequest, BeanScopeSession:
		return nil
	default:
		return fmt.Errorf(
			"bean metadata scope %q is unsupported",
			metadata.Scope,
		)
	}
}

func requirePayload[T any](payload *T, name string) error {
	if payload == nil {
		return fmt.Errorf(
			"annotation contribution kind %q requires its typed payload",
			name,
		)
	}
	return nil
}

func validateOptionalPrefix(name, value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf(
			"annotation %s prefix must not contain surrounding whitespace",
			name,
		)
	}
	for segment := range strings.SplitSeq(value, ".") {
		if !validIdentifier(segment) {
			return fmt.Errorf(
				"annotation %s prefix %q is not dot-separated identifiers",
				name,
				value,
			)
		}
	}
	return nil
}

func validateOptionalRoutePrefix(value string) error {
	if value == "" {
		return nil
	}
	if value != strings.TrimSpace(value) || !strings.HasPrefix(value, "/") {
		return fmt.Errorf(
			"annotation controller prefix %q must be an absolute trimmed route path",
			value,
		)
	}
	return nil
}

func validateRoute(route RouteContribution) error {
	if err := requireTrimmed("route method", route.Method); err != nil {
		return err
	}
	if strings.ToUpper(route.Method) != route.Method {
		return fmt.Errorf(
			"annotation route method %q must be uppercase",
			route.Method,
		)
	}
	if err := requireTrimmed("route path", route.Path); err != nil {
		return err
	}
	if !strings.HasPrefix(route.Path, "/") {
		return fmt.Errorf(
			"annotation route path %q must start with '/'",
			route.Path,
		)
	}
	return nil
}

func validateBootstrap(bootstrap BootstrapContribution) error {
	if err := requireTrimmed(
		"bootstrap capability",
		bootstrap.Capability,
	); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(bootstrap.Options))
	for _, option := range bootstrap.Options {
		if err := requireTrimmed(
			"bootstrap option name",
			option.Name,
		); err != nil {
			return err
		}
		if _, duplicate := seen[option.Name]; duplicate {
			return fmt.Errorf(
				"annotation bootstrap option %q is duplicated",
				option.Name,
			)
		}
		seen[option.Name] = struct{}{}
		if err := validateContributionValue(option.Value); err != nil {
			return fmt.Errorf(
				"annotation bootstrap option %q: %w",
				option.Name,
				err,
			)
		}
	}
	return nil
}

func validateContributionValue(value ContributionValue) error {
	switch value.Kind {
	case KindString:
		return nil
	case KindInteger:
		return nil
	case KindBoolean:
		return nil
	case KindIdentifier:
		return requireTrimmed(
			"contribution identifier",
			value.Identifier,
		)
	case KindList:
		for index, item := range value.List {
			if err := validateContributionValue(item); err != nil {
				return fmt.Errorf(
					"contribution list item %d: %w",
					index,
					err,
				)
			}
		}
		return nil
	default:
		return fmt.Errorf(
			"contribution value kind %q is unsupported",
			value.Kind,
		)
	}
}

func validateAuthorization(
	authorization AuthorizationContribution,
) error {
	for name, values := range map[string][]string{
		"any role":  authorization.AnyRoles,
		"all role":  authorization.AllRoles,
		"all scope": authorization.AllScopes,
	} {
		if err := validateUniqueTrimmed(name, values); err != nil {
			return err
		}
	}
	if authorization.Expression != "" &&
		authorization.Expression != strings.TrimSpace(authorization.Expression) {
		return errors.New(
			"annotation authorization expression must not have surrounding whitespace",
		)
	}
	return nil
}

func validateGeneratedFile(file GeneratedFileContribution) error {
	if err := requireTrimmed("generated file path", file.Path); err != nil {
		return err
	}
	if strings.Contains(file.Path, "\\") ||
		strings.HasPrefix(file.Path, "/") ||
		path.Clean(file.Path) != file.Path ||
		file.Path == ".." ||
		strings.HasPrefix(file.Path, "../") {
		return fmt.Errorf(
			"annotation generated file path %q must be a clean relative slash path",
			file.Path,
		)
	}
	if file.Content == "" {
		return errors.New(
			"annotation generated file contribution requires content",
		)
	}
	return nil
}

func requireTrimmed(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) {
		return fmt.Errorf(
			"annotation %s must be non-empty without surrounding whitespace",
			name,
		)
	}
	return nil
}

func validateUniqueTrimmed(name string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := requireTrimmed(name, value); err != nil {
			return err
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf(
				"annotation %s %q is duplicated",
				name,
				value,
			)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

func cloneBootstrapOptions(
	values []BootstrapOption,
) []BootstrapOption {
	if values == nil {
		return nil
	}
	result := make([]BootstrapOption, len(values))
	for index, value := range values {
		result[index] = value
		result[index].Value = cloneContributionValue(value.Value)
	}
	return result
}

func cloneContributionValue(value ContributionValue) ContributionValue {
	items := value.List
	value.List = make([]ContributionValue, len(items))
	for index, item := range items {
		value.List[index] = cloneContributionValue(item)
	}
	return value
}
