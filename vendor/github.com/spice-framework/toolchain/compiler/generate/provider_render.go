package generate

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/configuration"
	compilerevent "github.com/spice-framework/toolchain/compiler/event"
	"github.com/spice-framework/toolchain/compiler/provider"
)

func writeProviders(
	source *bytes.Buffer,
	providers []provider.Provider,
	componentFields []generatedComponentField,
	configTypes []configuration.Type,
	aliases map[string]string,
	dependencies map[string][]string,
	providerModules map[string]string,
	providerVariables map[string]string,
	events []compilerevent.Topic,
	adapters map[string]providerSourceAdapter,
) error {
	configByProvider := configurationProviderIndex(configTypes)
	eventByProvider := eventProviderIndex(events)
	overrideFields := overrideFieldIndex(componentFields)
	for _, item := range providers {
		variable := providerVariables[item.SymbolID]
		if item.Scope != sdk.BeanScopeSingleton {
			adapter, found := adapters[item.SymbolID]
			if !found {
				return fmt.Errorf(
					"scoped provider %s has no generated source adapter",
					item.SymbolID,
				)
			}
			writeScopedProviderAdapter(
				source,
				item,
				variable,
				aliases,
				dependencies[item.SymbolID],
				adapter,
			)
			continue
		}
		switch item.Source {
		case provider.SourceBean,
			provider.SourceStarter,
			provider.SourceAutoConfiguration:
			adapter, found := adapters[item.SymbolID]
			if !found {
				return fmt.Errorf(
					"provider %s has no generated source adapter",
					item.SymbolID,
				)
			}
			writeProviderAdapterCall(
				source,
				item,
				variable,
				dependencies[item.SymbolID],
				providerModules[item.SymbolID],
				adapter,
				overrideFields[item.SymbolID],
				aliases,
			)
		case provider.SourceStereotype:
			adapter, found := adapters[item.SymbolID]
			if !found {
				return fmt.Errorf(
					"provider %s has no generated source adapter",
					item.SymbolID,
				)
			}
			writeProviderAdapterCall(
				source,
				item,
				variable,
				dependencies[item.SymbolID],
				providerModules[item.SymbolID],
				adapter,
				overrideFields[item.SymbolID],
				aliases,
			)
		case provider.SourceConfiguration:
			configType, ok := configByProvider[item.SymbolID]
			if !ok {
				return fmt.Errorf("configuration provider %s has no typed configuration metadata", item.SymbolID)
			}
			adapter, found := adapters[item.SymbolID]
			if !found {
				return fmt.Errorf(
					"configuration provider %s has no generated source adapter",
					item.SymbolID,
				)
			}
			writeConfigurationAdapterCall(
				source,
				item,
				configType,
				variable,
				adapter,
			)
		case provider.SourceEvent:
			topic, ok := eventByProvider[item.SymbolID]
			if !ok {
				return fmt.Errorf(
					"event provider %s has no typed event metadata",
					item.SymbolID,
				)
			}
			if err := writeEventProvider(
				source,
				topic,
				variable,
				aliases,
				providerVariables,
			); err != nil {
				return err
			}
		default:
			return fmt.Errorf("provider %s has unsupported source %q", item.SymbolID, item.Source)
		}
	}
	return nil
}

func overrideFieldIndex(
	fields []generatedComponentField,
) map[string]string {
	result := make(map[string]string)
	for _, field := range fields {
		if field.overridable {
			result[field.providerID] = field.fieldName
		}
	}
	return result
}

func writeScopedProviderAdapter(
	source *bytes.Buffer,
	item provider.Provider,
	variable string,
	aliases map[string]string,
	dependencies []string,
	adapter providerSourceAdapter,
) {
	factory := variable + "Factory"
	outputType := renderedType(item.Output, aliases)
	fmt.Fprintf(
		source,
		"\t%s := func(_ context.Context) (%s, spicelifecycle.Cleanup, error) {\n",
		factory,
		outputType,
	)
	fmt.Fprintf(
		source,
		"\t\treturn %s.%s(%s)\n",
		adapter.alias,
		adapter.function,
		strings.Join(dependencies, ", "),
	)
	source.WriteString("\t}\n")
	switch item.Scope {
	case sdk.BeanScopeSingleton:
		return
	case sdk.BeanScopePrototype:
		fmt.Fprintf(
			source,
			"\t%s := spicebean.NewProvider(%s)\n",
			variable,
			factory,
		)
	case sdk.BeanScopeRequest, sdk.BeanScopeSession:
		scopeKind := "spicebean.ScopeRequest"
		if item.Scope == sdk.BeanScopeSession {
			scopeKind = "spicebean.ScopeSession"
		}
		fmt.Fprintf(
			source,
			"\t%sScope := spicebean.NewScoped[%s](%s, %s)\n",
			variable,
			outputType,
			scopeKind,
			factory,
		)
		fmt.Fprintf(
			source,
			"\t%s := %sScope.Provider()\n",
			variable,
			variable,
		)
	}
	fmt.Fprintf(source, "\t_ = %s\n", variable)
}

func writeProviderAdapterCall(
	source *bytes.Buffer,
	item provider.Provider,
	variable string,
	dependencies []string,
	moduleID string,
	adapter providerSourceAdapter,
	overrideField string,
	aliases map[string]string,
) {
	cleanup := variable + "Cleanup"
	if overrideField == "" {
		fmt.Fprintf(
			source,
			"\t%s, %s, err := %s.%s(%s)\n",
			variable,
			cleanup,
			adapter.alias,
			adapter.function,
			strings.Join(dependencies, ", "),
		)
	} else {
		fmt.Fprintf(
			source,
			"\t%s, %s, err := func() (%s, spicelifecycle.Cleanup, error) {\n",
			variable,
			cleanup,
			renderedType(item.Output, aliases),
		)
		fmt.Fprintf(
			source,
			"\t\tif options.Overrides.%s.Enabled() {\n",
			overrideField,
		)
		fmt.Fprintf(
			source,
			"\t\t\treturn options.Overrides.%s.Acquire(ctx)\n",
			overrideField,
		)
		source.WriteString("\t\t}\n")
		fmt.Fprintf(
			source,
			"\t\treturn %s.%s(%s)\n",
			adapter.alias,
			adapter.function,
			strings.Join(dependencies, ", "),
		)
		source.WriteString("\t}()\n")
	}
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
		strconv.Quote(
			"construct bean "+item.Name+
				" ("+item.OutputTypeID+
				", source "+item.SymbolID+"): %w",
		),
	)
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\tif %s != nil {\n", cleanup)
	fmt.Fprintf(
		source,
		"\t\tif err := application.coordinator.RegisterModuleCleanup(%s, %s, %s); err != nil {\n",
		strconv.Quote(moduleID),
		strconv.Quote(item.SymbolID),
		cleanup,
	)
	fmt.Fprintf(
		source,
		"\t\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
		strconv.Quote(
			"register cleanup for bean "+item.Name+
				" (source "+item.SymbolID+"): %w",
		),
	)
	source.WriteString("\t\t}\n")
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\t_ = %s\n", variable)
}

func writeConfigurationAdapterCall(
	source *bytes.Buffer,
	item provider.Provider,
	configType configuration.Type,
	variable string,
	adapter providerSourceAdapter,
) {
	fmt.Fprintf(
		source,
		"\t%s, err := %s.%s(configurationSnapshot)\n",
		variable,
		adapter.alias,
		adapter.function,
	)
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
		strconv.Quote(
			"bind configuration "+configType.TypeID+
				" for bean "+item.Name+
				" (source "+item.SymbolID+"): %w",
		),
	)
	source.WriteString("\t}\n")
	fmt.Fprintf(source, "\t_ = %s\n", variable)
}

func eventProviderIndex(events []compilerevent.Topic) map[string]compilerevent.Topic {
	result := make(map[string]compilerevent.Topic, len(events))
	for _, topic := range events {
		result[topic.ProviderID] = topic
	}
	return result
}

func writeEventProvider(
	source *bytes.Buffer,
	topic compilerevent.Topic,
	variable string,
	aliases map[string]string,
	providerVariables map[string]string,
) error {
	payload := renderedType(topic.Payload, aliases)
	topicVariable := variable + "Topic"
	fmt.Fprintf(source, "\t%s, err := spiceevent.NewTopic(\n", topicVariable)
	source.WriteString("\t\tspiceevent.Definition{\n")
	fmt.Fprintf(source, "\t\t\tID: %s,\n", strconv.Quote(topic.MarkerID))
	fmt.Fprintf(source, "\t\t\tModule: %s,\n", strconv.Quote(topic.Module))
	source.WriteString("\t\t},\n")
	fmt.Fprintf(source, "\t\t[]spiceevent.Subscriber[%s]{\n", payload)
	for _, listener := range topic.Listeners() {
		receiver := providerVariables[listener.ProviderID]
		if receiver == "" {
			return fmt.Errorf(
				"event listener %s references unknown provider %s",
				listener.MethodID,
				listener.ProviderID,
			)
		}
		source.WriteString("\t\t\t{\n")
		fmt.Fprintf(source, "\t\t\t\tID: %s,\n", strconv.Quote(listener.MethodID))
		fmt.Fprintf(source, "\t\t\t\tModule: %s,\n", strconv.Quote(listener.Module))
		if listener.Order != 0 {
			fmt.Fprintf(source, "\t\t\t\tOrder: %d,\n", listener.Order)
		}
		fmt.Fprintf(
			source,
			"\t\t\t\tHandle: %s.%s,\n",
			receiver,
			listener.Method.Name,
		)
		source.WriteString("\t\t\t},\n")
	}
	source.WriteString("\t\t},\n")
	source.WriteString("\t\toptions.EventObservers...,\n")
	source.WriteString("\t)\n")
	source.WriteString("\tif err != nil {\n")
	fmt.Fprintf(
		source,
		"\t\treturn nil, application.coordinator.Abort(ctx, fmt.Errorf(%s, err))\n",
		strconv.Quote(
			"construct event topic "+topic.MarkerID+
				" ("+topic.PublisherTypeID+"): %w",
		),
	)
	source.WriteString("\t}\n")
	fmt.Fprintf(
		source,
		"\tvar %s spiceevent.Publisher[%s] = %s\n",
		variable,
		payload,
		topicVariable,
	)
	fmt.Fprintf(source, "\t_ = %s\n", variable)
	return nil
}
