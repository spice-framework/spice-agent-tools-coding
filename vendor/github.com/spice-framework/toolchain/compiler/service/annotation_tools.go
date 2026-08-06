package service

import (
	"context"
	"encoding/json"
	"fmt"
	"go/token"
	"sort"
	"strings"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/spice/annotation/sdk/protocol"
	"github.com/spice-framework/toolchain/compiler/annotationhost"
	"github.com/spice-framework/toolchain/compiler/descriptor"
	"github.com/spice-framework/toolchain/compiler/diagnostic"
	"github.com/spice-framework/toolchain/compiler/load"
	"github.com/spice-framework/toolchain/compiler/provider"
	"github.com/spice-framework/toolchain/compiler/resolve"
)

func (service *Service) applyToolContributions(
	ctx context.Context,
	request normalizedRequest,
	program *load.Program,
	resolution resolve.Result,
	state preparedDescriptors,
) (resolve.Result, diagnostic.Set, error) {
	symbols := toolSymbolIndex(program)
	clients := make(map[string]*annotationhost.Client)
	var diagnostics []diagnostic.Diagnostic
	for index, occurrence := range resolution.Occurrences {
		if occurrence.Definition == (annotation.DefinitionReference{}) {
			continue
		}
		item, found := state.descriptors[occurrence.Definition]
		if !found {
			return resolution, diagnostic.NewSet(), fmt.Errorf(
				"resolved annotation %s has no decoded descriptor",
				occurrence.Spelling,
			)
		}
		client, err := service.annotationToolClient(
			ctx,
			request,
			item,
			clients,
		)
		if err != nil {
			diagnostics = append(
				diagnostics,
				toolFailureDiagnostic(request.root, occurrence, err),
			)
			break
		}
		params, err := toolAnalyzeParams(
			item,
			occurrence,
			symbols[occurrence.SymbolID],
		)
		if err != nil {
			return resolution, diagnostic.NewSet(), err
		}
		analyzed, err := client.Analyze(ctx, params)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return resolution, diagnostic.NewSet(), contextErr
			}
			diagnostics = append(
				diagnostics,
				toolFailureDiagnostic(request.root, occurrence, err),
			)
			break
		}
		contributions, contributionErr := decodeToolContributions(
			analyzed.Contributions,
		)
		if contributionErr != nil {
			diagnostics = append(
				diagnostics,
				toolFailureDiagnostic(
					request.root,
					occurrence,
					contributionErr,
				),
			)
			break
		}
		resolution, err = resolution.WithContributions(
			index,
			contributions,
		)
		if err != nil {
			diagnostics = append(
				diagnostics,
				toolFailureDiagnostic(request.root, occurrence, err),
			)
			break
		}
		toolDiagnostics, err := toolResultDiagnostics(
			request.root,
			occurrence,
			analyzed.Diagnostics,
		)
		if err != nil {
			diagnostics = append(
				diagnostics,
				toolFailureDiagnostic(request.root, occurrence, err),
			)
			break
		}
		diagnostics = append(diagnostics, toolDiagnostics...)
	}
	return resolution, diagnostic.NewSet(diagnostics...), nil
}

func (service *Service) annotationToolClient(
	ctx context.Context,
	request normalizedRequest,
	item descriptor.Descriptor,
	clients map[string]*annotationhost.Client,
) (*annotationhost.Client, error) {
	implementation := item.Definition.Implementation
	client := clients[implementation.Tool]
	if client == nil {
		var err error
		client, err = service.config.annotationTools.Client(
			ctx,
			annotationhost.Config{
				Root:         request.root,
				ToolPath:     implementation.Tool,
				SpiceVersion: service.config.spiceVersion,
				Environment:  service.config.loadOptions.Env,
			},
		)
		if err != nil {
			return nil, err
		}
		clients[implementation.Tool] = client
	}
	if err := client.ValidateDescriptor(
		item.Package,
		item.Symbol,
		item.Definition,
		item.Provenance,
	); err != nil {
		return nil, err
	}
	return client, nil
}

func toolSymbolIndex(program *load.Program) map[string]load.Symbol {
	result := make(map[string]load.Symbol)
	if program == nil {
		return result
	}
	for _, symbol := range program.Symbols() {
		result[symbol.ID] = symbol
	}
	return result
}

func toolAnalyzeParams(
	item descriptor.Descriptor,
	occurrence resolve.Occurrence,
	symbol load.Symbol,
) (protocol.AnalyzeParams, error) {
	arguments := make(
		[]protocol.Argument,
		len(occurrence.Annotation.Arguments),
	)
	for index, argument := range occurrence.Annotation.Arguments {
		content, err := toolArgumentJSON(argument.Value)
		if err != nil {
			return protocol.AnalyzeParams{}, err
		}
		arguments[index] = protocol.Argument{
			Name:       argument.Name,
			Kind:       argument.Value.Kind,
			Positional: argument.Name == "",
			Value:      content,
		}
	}
	typeID := ""
	if symbol.Object != nil {
		typeID = provider.TypeID(symbol.Object.Type())
	}
	parameterTypeID := ""
	if occurrence.Target == annotation.TargetParameter &&
		symbol.Signature != nil &&
		occurrence.ParameterIndex >= 0 &&
		occurrence.ParameterIndex < symbol.Signature.Params().Len() {
		parameterTypeID = provider.TypeID(
			symbol.Signature.Params().At(
				occurrence.ParameterIndex,
			).Type(),
		)
	}
	facts := map[string]string{
		"symbol_kind": string(symbol.Kind),
	}
	if symbol.Receiver != "" {
		facts["receiver"] = symbol.Receiver
	}
	return protocol.AnalyzeParams{
		Descriptor: sdk.Symbol{
			Package: item.Package,
			Name:    item.Symbol,
		},
		Invocation: protocol.Invocation{
			DescriptorPackage: item.Package,
			DescriptorSymbol:  item.Symbol,
			CanonicalName:     item.Definition.Name,
			Arguments:         arguments,
			Declaration: protocol.Declaration{
				Target:          occurrence.Target,
				SymbolID:        occurrence.SymbolID,
				Name:            occurrence.Name,
				PackagePath:     occurrence.PackagePath,
				TypeID:          typeID,
				ParameterIndex:  occurrence.ParameterIndex,
				ParameterName:   occurrence.ParameterName,
				ParameterTypeID: parameterTypeID,
			},
			Facts: facts,
		},
	}, nil
}

func toolArgumentJSON(value annotation.Value) (json.RawMessage, error) {
	normalized, err := normalizedToolValue(value)
	if err != nil {
		return nil, err
	}
	content, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf(
			"encode annotation argument value: %w",
			err,
		)
	}
	return content, nil
}

func normalizedToolValue(value annotation.Value) (any, error) {
	switch value.Kind {
	case annotation.KindString:
		return value.String, nil
	case annotation.KindInteger:
		return value.Integer, nil
	case annotation.KindBoolean:
		return value.Boolean, nil
	case annotation.KindIdentifier:
		return value.Identifier, nil
	case annotation.KindList:
		result := make([]any, len(value.List))
		for index, item := range value.List {
			normalized, err := normalizedToolValue(item)
			if err != nil {
				return nil, err
			}
			result[index] = normalized
		}
		return result, nil
	default:
		return nil, fmt.Errorf(
			"annotation argument has unsupported value kind %q",
			value.Kind,
		)
	}
}

func decodeToolContributions(
	values []protocol.Contribution,
) ([]sdk.Contribution, error) {
	result := make([]sdk.Contribution, len(values))
	for index, value := range values {
		decoded, err := protocol.DecodeContribution(value)
		if err != nil {
			return nil, fmt.Errorf(
				"annotation tool contribution %d: %w",
				index,
				err,
			)
		}
		result[index] = decoded
	}
	sort.SliceStable(result, func(left, right int) bool {
		return result[left].Kind < result[right].Kind
	})
	return result, nil
}

func toolResultDiagnostics(
	root string,
	occurrence resolve.Occurrence,
	values []protocol.Diagnostic,
) ([]diagnostic.Diagnostic, error) {
	result := make([]diagnostic.Diagnostic, len(values))
	for index, value := range values {
		if strings.TrimSpace(value.Code) == "" ||
			strings.TrimSpace(value.Message) == "" {
			return nil, fmt.Errorf(
				"annotation tool diagnostic %d requires code and message",
				index,
			)
		}
		severity, err := toolDiagnosticSeverity(value.Severity)
		if err != nil {
			return nil, fmt.Errorf(
				"annotation tool diagnostic %d: %w",
				index,
				err,
			)
		}
		result[index] = diagnostic.New(
			diagnostic.Code("annotation-tool", value.Code),
			severity,
			value.Message,
			toolDiagnosticLocation(root, occurrence),
		)
	}
	return result, nil
}

func toolDiagnosticSeverity(
	value string,
) (diagnostic.Severity, error) {
	switch diagnostic.Severity(value) {
	case diagnostic.SeverityError:
		return diagnostic.SeverityError, nil
	case diagnostic.SeverityWarning:
		return diagnostic.SeverityWarning, nil
	case diagnostic.SeverityInformation:
		return diagnostic.SeverityInformation, nil
	case diagnostic.SeverityHint:
		return diagnostic.SeverityHint, nil
	default:
		return "", fmt.Errorf(
			"annotation tool diagnostic severity %q is unsupported",
			value,
		)
	}
}

func toolFailureDiagnostic(
	root string,
	occurrence resolve.Occurrence,
	err error,
) diagnostic.Diagnostic {
	return diagnostic.New(
		diagnostic.Code("annotation-tool", "operation"),
		diagnostic.SeverityError,
		err.Error(),
		toolDiagnosticLocation(root, occurrence),
	)
}

func toolDiagnosticLocation(
	root string,
	occurrence resolve.Occurrence,
) diagnostic.Location {
	display := occurrence.DisplayPosition
	physical := occurrence.PhysicalPosition
	if physical.Filename == "" {
		physical = token.Position{
			Filename: occurrence.PhysicalFile,
			Offset:   occurrence.PhysicalOffset,
		}
	}
	return diagnostic.SourceMappedLocation(
		root,
		display.Filename,
		physical.Filename,
		display.Line,
		display.Column,
		display.Offset,
		physical.Line,
		physical.Column,
		physical.Offset,
	)
}
