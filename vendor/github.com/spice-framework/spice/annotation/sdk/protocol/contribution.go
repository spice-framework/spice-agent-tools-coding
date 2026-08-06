package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spice-framework/spice/annotation/sdk"
)

// EncodeContribution validates and serializes one typed SDK contribution for
// transport. Annotation tools should use this boundary instead of hand-writing
// raw JSON envelopes.
func EncodeContribution(
	value sdk.Contribution,
) (Contribution, error) {
	if err := value.Validate(); err != nil {
		return Contribution{}, err
	}
	payload, err := contributionPayload(value)
	if err != nil {
		return Contribution{}, err
	}
	content, err := json.Marshal(payload)
	if err != nil {
		return Contribution{}, fmt.Errorf(
			"encode annotation contribution %q: %w",
			value.Kind,
			err,
		)
	}
	return Contribution{Kind: value.Kind, Value: content}, nil
}

// DecodeContribution strictly decodes and validates one wire contribution.
// Unknown fields, trailing JSON, missing payloads, and invalid values fail
// before the contribution can enter compiler state.
func DecodeContribution(
	wire Contribution,
) (sdk.Contribution, error) {
	value, destination, err := contributionDestination(wire.Kind)
	if err != nil {
		return sdk.Contribution{}, err
	}
	if len(wire.Value) == 0 {
		return sdk.Contribution{}, fmt.Errorf(
			"annotation contribution %q requires a JSON value",
			wire.Kind,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(wire.Value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return sdk.Contribution{}, fmt.Errorf(
			"decode annotation contribution %q: %w",
			wire.Kind,
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return sdk.Contribution{}, fmt.Errorf(
			"decode annotation contribution %q: %w",
			wire.Kind,
			err,
		)
	}
	if err := value.Validate(); err != nil {
		return sdk.Contribution{}, fmt.Errorf(
			"validate annotation contribution %q: %w",
			wire.Kind,
			err,
		)
	}
	return value, nil
}

func contributionPayload(value sdk.Contribution) (any, error) {
	if payload, found := foundationContributionPayload(value); found {
		return payload, nil
	}
	if payload, found := integrationContributionPayload(value); found {
		return payload, nil
	}
	return nil, fmt.Errorf(
		"annotation contribution kind %q is unsupported",
		value.Kind,
	)
}

func foundationContributionPayload(
	value sdk.Contribution,
) (any, bool) {
	//nolint:exhaustive // Wire payload cases are deliberately partitioned by domain.
	switch value.Kind {
	case sdk.ContributionApplication:
		return value.Application, true
	case sdk.ContributionStereotype:
		return value.Stereotype, true
	case sdk.ContributionInterface:
		return value.Interface, true
	case sdk.ContributionProvider:
		return value.Provider, true
	case sdk.ContributionBeanMetadata:
		return value.BeanMetadata, true
	case sdk.ContributionConfiguration:
		return value.Configuration, true
	case sdk.ContributionController:
		return value.Controller, true
	case sdk.ContributionRoute:
		return value.Route, true
	case sdk.ContributionModule:
		return value.Module, true
	case sdk.ContributionNamedInterface:
		return value.NamedInterface, true
	case sdk.ContributionLifecycle:
		return value.Lifecycle, true
	case sdk.ContributionBootstrap:
		return value.Bootstrap, true
	default:
		return nil, false
	}
}

func integrationContributionPayload(
	value sdk.Contribution,
) (any, bool) {
	//nolint:exhaustive // Wire payload cases are deliberately partitioned by domain.
	switch value.Kind {
	case sdk.ContributionSchedule:
		return value.Schedule, true
	case sdk.ContributionAsync:
		return value.Async, true
	case sdk.ContributionTransaction:
		return value.Transaction, true
	case sdk.ContributionEventTopic:
		return value.EventTopic, true
	case sdk.ContributionEventListener:
		return value.EventListener, true
	case sdk.ContributionCache:
		return value.Cache, true
	case sdk.ContributionAuthorization:
		return value.Authorization, true
	case sdk.ContributionGeneratedFile:
		return value.GeneratedFile, true
	default:
		return nil, false
	}
}

func contributionDestination(
	kind sdk.ContributionKind,
) (sdk.Contribution, any, error) {
	if value, destination, found := foundationContributionDestination(kind); found {
		return value, destination, nil
	}
	if value, destination, found := integrationContributionDestination(kind); found {
		return value, destination, nil
	}
	return sdk.Contribution{}, nil, fmt.Errorf(
		"annotation contribution kind %q is unsupported",
		kind,
	)
}

func foundationContributionDestination(
	kind sdk.ContributionKind,
) (sdk.Contribution, any, bool) {
	value := sdk.Contribution{Kind: kind}
	//nolint:exhaustive // Wire destination cases are deliberately partitioned by domain.
	switch kind {
	case sdk.ContributionApplication:
		value.Application = &sdk.ApplicationContribution{}
		return value, value.Application, true
	case sdk.ContributionStereotype:
		value.Stereotype = &sdk.StereotypeContribution{}
		return value, value.Stereotype, true
	case sdk.ContributionInterface:
		value.Interface = &sdk.InterfaceBindingContribution{}
		return value, value.Interface, true
	case sdk.ContributionProvider:
		value.Provider = &sdk.ProviderContribution{}
		return value, value.Provider, true
	case sdk.ContributionBeanMetadata:
		value.BeanMetadata = &sdk.BeanMetadataContribution{}
		return value, value.BeanMetadata, true
	case sdk.ContributionConfiguration:
		value.Configuration = &sdk.ConfigurationContribution{}
		return value, value.Configuration, true
	case sdk.ContributionController:
		value.Controller = &sdk.ControllerContribution{}
		return value, value.Controller, true
	case sdk.ContributionRoute:
		value.Route = &sdk.RouteContribution{}
		return value, value.Route, true
	case sdk.ContributionModule:
		value.Module = &sdk.ModuleContribution{}
		return value, value.Module, true
	case sdk.ContributionNamedInterface:
		value.NamedInterface = &sdk.NamedInterfaceContribution{}
		return value, value.NamedInterface, true
	case sdk.ContributionLifecycle:
		value.Lifecycle = &sdk.LifecycleContribution{}
		return value, value.Lifecycle, true
	case sdk.ContributionBootstrap:
		value.Bootstrap = &sdk.BootstrapContribution{}
		return value, value.Bootstrap, true
	default:
		return sdk.Contribution{}, nil, false
	}
}

func integrationContributionDestination(
	kind sdk.ContributionKind,
) (sdk.Contribution, any, bool) {
	value := sdk.Contribution{Kind: kind}
	//nolint:exhaustive // Wire destination cases are deliberately partitioned by domain.
	switch kind {
	case sdk.ContributionSchedule:
		value.Schedule = &sdk.ScheduleContribution{}
		return value, value.Schedule, true
	case sdk.ContributionAsync:
		value.Async = &sdk.AsyncContribution{}
		return value, value.Async, true
	case sdk.ContributionTransaction:
		value.Transaction = &sdk.TransactionContribution{}
		return value, value.Transaction, true
	case sdk.ContributionEventTopic:
		value.EventTopic = &sdk.EventTopicContribution{}
		return value, value.EventTopic, true
	case sdk.ContributionEventListener:
		value.EventListener = &sdk.EventListenerContribution{}
		return value, value.EventListener, true
	case sdk.ContributionCache:
		value.Cache = &sdk.CacheContribution{}
		return value, value.Cache, true
	case sdk.ContributionAuthorization:
		value.Authorization = &sdk.AuthorizationContribution{}
		return value, value.Authorization, true
	case sdk.ContributionGeneratedFile:
		value.GeneratedFile = &sdk.GeneratedFileContribution{}
		return value, value.GeneratedFile, true
	default:
		return sdk.Contribution{}, nil, false
	}
}
