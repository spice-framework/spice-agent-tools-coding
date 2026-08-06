package generate

import (
	"bytes"
	"fmt"
	"go/types"
	"path"
	"strconv"

	"github.com/spice-framework/toolchain/compiler/controller"
)

type generatedRouteInterceptorField struct {
	routeID   string
	fieldName string
	request   types.Type
	response  types.Type
}

func generatedRouteInterceptorFields(
	controllers []controller.Controller,
) []generatedRouteInterceptorField {
	type candidate struct {
		route controller.Route
		base  string
	}
	var candidates []candidate
	counts := make(map[string]int)
	for _, owner := range controllers {
		for _, route := range owner.Routes() {
			if !interceptableRoute(route) {
				continue
			}
			base := exportedGeneratedIdentifier(
				route.Symbol.Receiver,
				"Controller",
			) + exportedGeneratedIdentifier(route.Name, "Method")
			counts[base]++
			candidates = append(candidates, candidate{route: route, base: base})
		}
	}
	used := make(map[string]int)
	result := make([]generatedRouteInterceptorField, 0, len(candidates))
	for _, candidate := range candidates {
		base := candidate.base
		if counts[base] > 1 {
			base = exportedGeneratedIdentifier(
				path.Base(candidate.route.Symbol.PackagePath),
				"Package",
			) + base
		}
		used[base]++
		fieldName := base
		if used[base] > 1 {
			fieldName += strconv.Itoa(used[base])
		}
		result = append(result, generatedRouteInterceptorField{
			routeID:   candidate.route.SymbolID,
			fieldName: fieldName,
			request:   candidate.route.Request,
			response:  candidate.route.Response,
		})
	}
	return result
}

func interceptableRoute(route controller.Route) bool {
	return !route.Raw && !route.BindingResult
}

func routeInterceptorFieldIndex(
	fields []generatedRouteInterceptorField,
) map[string]generatedRouteInterceptorField {
	result := make(map[string]generatedRouteInterceptorField, len(fields))
	for _, field := range fields {
		result[field.routeID] = field
	}
	return result
}

func writeRouteInterceptorsType(
	source *bytes.Buffer,
	fields []generatedRouteInterceptorField,
	aliases map[string]string,
) {
	if len(fields) == 0 {
		return
	}
	source.WriteString("// RouteInterceptors configures typed generated route method decorators.\n")
	source.WriteString("// The first interceptor in each field is the outermost invocation.\n")
	source.WriteString("type RouteInterceptors struct {\n")
	for _, field := range fields {
		fmt.Fprintf(
			source,
			"\t%s []spiceintercept.Interceptor[%s, %s]\n",
			field.fieldName,
			renderedType(field.request, aliases),
			renderedType(field.response, aliases),
		)
	}
	source.WriteString("}\n\n")
}
