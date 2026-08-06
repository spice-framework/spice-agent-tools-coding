package generate

import (
	"bytes"
	"fmt"
	"go/token"
	"go/types"
	"path"
	"strconv"
	"strings"

	"github.com/spice-framework/spice/annotation/sdk"
	"github.com/spice-framework/toolchain/compiler/provider"
)

type generatedComponentField struct {
	providerID  string
	beanName    string
	fieldName   string
	output      types.Type
	overridable bool
}

func generatedComponentFields(
	providers []provider.Provider,
) []generatedComponentField {
	type candidate struct {
		provider provider.Provider
		index    int
		base     string
	}
	var candidates []candidate
	baseCounts := make(map[string]int)
	for index, item := range providers {
		if item.Scope != sdk.BeanScopeSingleton ||
			!publicComponentType(item.Output) {
			continue
		}
		base := componentFieldBase(item, index)
		baseCounts[base]++
		candidates = append(candidates, candidate{
			provider: item,
			index:    index,
			base:     base,
		})
	}

	used := make(map[string]int, len(candidates))
	fields := make([]generatedComponentField, 0, len(candidates))
	for _, candidate := range candidates {
		base := candidate.base
		if baseCounts[base] > 1 {
			prefix := exportedComponentFieldName(
				path.Base(candidate.provider.PackagePath),
				candidate.index,
			)
			base = prefix + base
		}
		used[base]++
		fieldName := base
		if used[base] > 1 {
			fieldName += strconv.Itoa(used[base])
		}
		fields = append(fields, generatedComponentField{
			providerID:  candidate.provider.SymbolID,
			beanName:    candidate.provider.Name,
			fieldName:   fieldName,
			output:      candidate.provider.Output,
			overridable: sourceUnitConstructsProvider(candidate.provider),
		})
	}
	return fields
}

func hasOverridableProviders(fields []generatedComponentField) bool {
	for _, field := range fields {
		if field.overridable {
			return true
		}
	}
	return false
}

func componentFieldBase(item provider.Provider, index int) string {
	base := exportedComponentFieldName(semanticProviderName(item), index)
	if item.ExplicitName ||
		!strings.HasPrefix(base, "New") ||
		len(base) == len("New") {
		return base
	}
	trimmed := base[len("New"):]
	if token.IsIdentifier(trimmed) {
		return trimmed
	}
	return base
}

func exportedComponentFieldName(name string, index int) string {
	if name == "" {
		return "Provider" + strconv.Itoa(index)
	}
	first := name[0]
	if first >= 'a' && first <= 'z' {
		first -= 'a' - 'A'
		name = string(first) + name[1:]
	}
	if !token.IsIdentifier(name) {
		return "Provider" + strconv.Itoa(index)
	}
	return name
}

func publicComponentType(value types.Type) bool {
	switch typed := value.(type) {
	case *types.Basic:
		return true
	case *types.Named:
		return publicNamedComponentType(typed.Obj(), typed.TypeArgs())
	case *types.Alias:
		return publicNamedComponentType(typed.Obj(), typed.TypeArgs())
	case *types.Pointer:
		return publicComponentType(typed.Elem())
	case *types.Slice:
		return publicComponentType(typed.Elem())
	case *types.Array:
		return publicComponentType(typed.Elem())
	case *types.Map:
		return publicComponentType(typed.Key()) &&
			publicComponentType(typed.Elem())
	case *types.Chan:
		return publicComponentType(typed.Elem())
	default:
		return false
	}
}

func publicNamedComponentType(
	object *types.TypeName,
	arguments *types.TypeList,
) bool {
	if object == nil ||
		object.Pkg() != nil && !object.Exported() {
		return false
	}
	return publicTypeArguments(arguments)
}

func publicTypeArguments(arguments *types.TypeList) bool {
	if arguments == nil {
		return true
	}
	for argument := range arguments.Types() {
		if !publicComponentType(argument) {
			return false
		}
	}
	return true
}

func writeComponentsType(
	source *bytes.Buffer,
	fields []generatedComponentField,
	aliases map[string]string,
) {
	source.WriteString("// Components is a typed snapshot of constructed singleton beans.\n")
	source.WriteString("// It performs no reflection or string-based lookup.\n")
	source.WriteString("type Components struct {\n")
	for _, field := range fields {
		fmt.Fprintf(
			source,
			"\t// %s is bean %q.\n",
			field.fieldName,
			field.beanName,
		)
		fmt.Fprintf(
			source,
			"\t%s %s\n",
			field.fieldName,
			renderedType(field.output, aliases),
		)
	}
	source.WriteString("}\n\n")
}

func writeBeanOverridesType(
	source *bytes.Buffer,
	fields []generatedComponentField,
	aliases map[string]string,
) {
	if !hasOverridableProviders(fields) {
		return
	}
	source.WriteString("// BeanOverrides provides compile-time-typed singleton replacements.\n")
	source.WriteString("// Replacements use the normal generated cleanup and rollback path.\n")
	source.WriteString("type BeanOverrides struct {\n")
	for _, field := range fields {
		if !field.overridable {
			continue
		}
		fmt.Fprintf(
			source,
			"\t// %s replaces bean %q.\n",
			field.fieldName,
			field.beanName,
		)
		fmt.Fprintf(
			source,
			"\t%s spicebean.Override[%s]\n",
			field.fieldName,
			renderedType(field.output, aliases),
		)
	}
	source.WriteString("}\n\n")
	writeBeanOverrideComposition(source, fields)
}

func writeBeanOverrideComposition(
	source *bytes.Buffer,
	fields []generatedComponentField,
) {
	source.WriteString("// BeanOverrideLayer is one named immutable override composition layer.\n")
	source.WriteString("// Layers are applied in order; a later layer deliberately replaces an earlier value.\n")
	source.WriteString("type BeanOverrideLayer struct {\n")
	source.WriteString("\tName string\n")
	source.WriteString("\tOverrides BeanOverrides\n")
	source.WriteString("}\n\n")
	source.WriteString("// ComposeBeanOverrides validates and deterministically composes named layers.\n")
	source.WriteString("// It never mutates a running application or performs runtime bean lookup.\n")
	source.WriteString("func ComposeBeanOverrides(layers ...BeanOverrideLayer) (BeanOverrides, error) {\n")
	source.WriteString("\tresult := BeanOverrides{}\n")
	source.WriteString("\tseen := make(map[string]int, len(layers))\n")
	source.WriteString("\tfor index, layer := range layers {\n")
	source.WriteString("\t\tif layer.Name == \"\" || strings.TrimSpace(layer.Name) != layer.Name {\n")
	source.WriteString("\t\t\treturn BeanOverrides{}, fmt.Errorf(\"compose bean overrides: layer %d requires a non-empty name without surrounding whitespace\", index)\n")
	source.WriteString("\t\t}\n")
	source.WriteString("\t\tif previous, duplicate := seen[layer.Name]; duplicate {\n")
	source.WriteString("\t\t\treturn BeanOverrides{}, fmt.Errorf(\"compose bean overrides: layer %q repeats layer %d\", layer.Name, previous)\n")
	source.WriteString("\t\t}\n")
	source.WriteString("\t\tseen[layer.Name] = index\n")
	for _, field := range fields {
		if !field.overridable {
			continue
		}
		fmt.Fprintf(
			source,
			"\t\tif layer.Overrides.%s.Enabled() {\n\t\t\tresult.%s = layer.Overrides.%s\n\t\t}\n",
			field.fieldName,
			field.fieldName,
			field.fieldName,
		)
	}
	source.WriteString("\t}\n")
	source.WriteString("\treturn result, nil\n")
	source.WriteString("}\n\n")
}

func writeComponentAssignments(
	source *bytes.Buffer,
	fields []generatedComponentField,
	providerVariables map[string]string,
) {
	if len(fields) == 0 {
		return
	}
	source.WriteString("\tapplication.components = Components{\n")
	for _, field := range fields {
		fmt.Fprintf(
			source,
			"\t\t%s: %s,\n",
			field.fieldName,
			providerVariables[field.providerID],
		)
	}
	source.WriteString("\t}\n")
}

func writeComponentsMethod(source *bytes.Buffer) {
	source.WriteString("// Components returns a typed snapshot of constructed singleton beans.\n")
	source.WriteString("func (application *Application) Components() Components {\n")
	source.WriteString("\tif application == nil {\n")
	source.WriteString("\t\treturn Components{}\n")
	source.WriteString("\t}\n")
	source.WriteString("\treturn application.components\n")
	source.WriteString("}\n\n")
}
