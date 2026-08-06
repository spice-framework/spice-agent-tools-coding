package descriptor

import (
	"fmt"
	"go/token"
	"go/types"
	"slices"

	"github.com/spice-framework/spice/annotation"
	"github.com/spice-framework/toolchain/compiler/load"
)

// NamespaceReferences discovers every exported descriptor function from
// explicitly namespace-imported packages in the existing typed program. It
// recognizes only exact func() sdk.Definition signatures and executes no code.
func NamespaceReferences(
	program *load.Program,
	packages []string,
) ([]annotation.DefinitionReference, error) {
	if program == nil {
		return nil, fmt.Errorf(
			"discover namespace annotation descriptors: program is nil",
		)
	}
	selected := make(map[string]struct{}, len(packages))
	for _, packagePath := range packages {
		selected[packagePath] = struct{}{}
	}
	found := make(map[string]int, len(selected))
	var references []annotation.DefinitionReference
	for _, symbol := range program.Symbols() {
		if _, wanted := selected[symbol.PackagePath]; !wanted ||
			symbol.Kind != load.SymbolFunction ||
			!token.IsExported(symbol.Name) ||
			!isDescriptorSignature(symbol.Signature) {
			continue
		}
		references = append(references, annotation.DefinitionReference{
			Package: symbol.PackagePath,
			Symbol:  symbol.Name,
		})
		found[symbol.PackagePath]++
	}
	for packagePath := range selected {
		if found[packagePath] == 0 {
			return nil, fmt.Errorf(
				"namespace annotation package %q exports no func() sdk.Definition descriptors",
				packagePath,
			)
		}
	}
	slices.SortFunc(
		references,
		func(left, right annotation.DefinitionReference) int {
			if left.Package < right.Package {
				return -1
			}
			if left.Package > right.Package {
				return 1
			}
			if left.Symbol < right.Symbol {
				return -1
			}
			if left.Symbol > right.Symbol {
				return 1
			}
			return 0
		},
	)
	return references, nil
}

func isDescriptorSignature(signature *types.Signature) bool {
	if signature == nil ||
		signature.Recv() != nil ||
		signature.TypeParams().Len() != 0 ||
		signature.Params().Len() != 0 ||
		signature.Results().Len() != 1 ||
		signature.Variadic() {
		return false
	}
	result, ok := types.Unalias(
		signature.Results().At(0).Type(),
	).(*types.Named)
	if !ok || result.Obj() == nil || result.Obj().Pkg() == nil {
		return false
	}
	return result.Obj().Pkg().Path() == sdkPackagePath &&
		result.Obj().Name() == "Definition"
}
