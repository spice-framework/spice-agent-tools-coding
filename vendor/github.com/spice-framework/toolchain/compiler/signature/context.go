// Package signature provides exact loaded-program signature identities shared
// by compiler feature analyzers.
package signature

import (
	"go/types"

	"github.com/spice-framework/toolchain/compiler/load"
)

var (
	errorType = types.Universe.Lookup("error").Type()
	anyType   = types.Universe.Lookup("any").Type()
)

// ContextType returns the canonical context.Context identity only when the
// loaded declaration exactly matches the Go 1.26 contract. A nil result means
// that the identity could not be established safely.
func ContextType(program *load.Program) types.Type {
	if program == nil {
		return nil
	}
	contextType := loadedNamedType(program, "context", "Context")
	timeType := loadedNamedType(program, "time", "Time")
	if contextType == nil ||
		timeType == nil ||
		!validContextDeclaration(contextType, timeType) {
		return nil
	}
	return contextType
}

func loadedNamedType(
	program *load.Program,
	packagePath string,
	typeName string,
) *types.Named {
	seen := make(map[*types.Package]struct{})
	var found *types.Named
	valid := true
	var visit func(*types.Package)
	visit = func(pkg *types.Package) {
		if pkg == nil {
			return
		}
		if _, ok := seen[pkg]; ok {
			return
		}
		seen[pkg] = struct{}{}
		if pkg.Path() == packagePath {
			object, ok := pkg.Scope().Lookup(typeName).(*types.TypeName)
			if !ok || object.IsAlias() {
				valid = false
			} else {
				named, ok := object.Type().(*types.Named)
				switch {
				case !ok || named.Obj() != object:
					valid = false
				case found == nil:
					found = named
				case !types.Identical(found, named):
					valid = false
				}
			}
		}
		for _, imported := range pkg.Imports() {
			visit(imported)
		}
	}
	for _, pkg := range program.Packages() {
		visit(pkg.Types)
	}
	if !valid {
		return nil
	}
	return found
}

func validContextDeclaration(
	contextType *types.Named,
	timeType *types.Named,
) bool {
	contract, ok := contextType.Underlying().(*types.Interface)
	if !ok {
		return false
	}
	contract.Complete()
	if contract.NumEmbeddeds() != 0 ||
		contract.NumExplicitMethods() != 4 ||
		contract.NumMethods() != 4 {
		return false
	}

	methods := make(map[string]*types.Signature, contract.NumMethods())
	for method := range contract.Methods() {
		methodSignature, valid := validContextMethod(method, contextType)
		if !valid {
			return false
		}
		if _, duplicate := methods[method.Name()]; duplicate {
			return false
		}
		methods[method.Name()] = methodSignature
	}

	return validDeadlineMethod(methods["Deadline"], timeType) &&
		validDoneMethod(methods["Done"]) &&
		validErrMethod(methods["Err"]) &&
		validValueMethod(methods["Value"])
}

func validContextMethod(
	method *types.Func,
	contextType *types.Named,
) (*types.Signature, bool) {
	if method.Pkg() != contextType.Obj().Pkg() {
		return nil, false
	}
	methodSignature, ok := method.Type().(*types.Signature)
	if !ok || methodSignature.Recv() == nil {
		return nil, false
	}
	if !types.Identical(methodSignature.Recv().Type(), contextType) ||
		methodSignature.Variadic() {
		return nil, false
	}
	if methodSignature.TypeParams() != nil &&
		methodSignature.TypeParams().Len() != 0 {
		return nil, false
	}
	if methodSignature.RecvTypeParams() != nil &&
		methodSignature.RecvTypeParams().Len() != 0 {
		return nil, false
	}
	return methodSignature, true
}

func validDeadlineMethod(
	methodSignature *types.Signature,
	timeType types.Type,
) bool {
	return hasArity(methodSignature, 0, 2) &&
		types.Identical(methodSignature.Results().At(0).Type(), timeType) &&
		types.Identical(
			methodSignature.Results().At(1).Type(),
			types.Typ[types.Bool],
		)
}

func validDoneMethod(methodSignature *types.Signature) bool {
	if !hasArity(methodSignature, 0, 1) {
		return false
	}
	channel, ok := methodSignature.Results().At(0).Type().(*types.Chan)
	if !ok || channel.Dir() != types.RecvOnly {
		return false
	}
	return types.Identical(channel.Elem(), types.NewStruct(nil, nil))
}

func validErrMethod(methodSignature *types.Signature) bool {
	return hasArity(methodSignature, 0, 1) &&
		types.Identical(methodSignature.Results().At(0).Type(), errorType)
}

func validValueMethod(methodSignature *types.Signature) bool {
	return hasArity(methodSignature, 1, 1) &&
		types.Identical(methodSignature.Params().At(0).Type(), anyType) &&
		types.Identical(methodSignature.Results().At(0).Type(), anyType)
}

func hasArity(
	methodSignature *types.Signature,
	parameters int,
	results int,
) bool {
	return methodSignature != nil &&
		methodSignature.Params().Len() == parameters &&
		methodSignature.Results().Len() == results
}
