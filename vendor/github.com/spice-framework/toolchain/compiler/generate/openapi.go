package generate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"go/types"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/spice-framework/toolchain/compiler/application"
	"github.com/spice-framework/toolchain/compiler/controller"
)

const openAPIFilename = "openapi.json"

var openAPINamePart = regexp.MustCompile(`[^A-Za-z0-9_]+`)

type openAPIDocument struct {
	OpenAPI    string                 `json:"openapi"`
	Info       openAPIInfo            `json:"info"`
	Paths      map[string]openAPIPath `json:"paths"`
	Components openAPIComponents      `json:"components"`
}

type openAPIInfo struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

type openAPIComponents struct {
	Schemas         map[string]openAPISchema         `json:"schemas"`
	SecuritySchemes map[string]openAPISecurityScheme `json:"securitySchemes,omitempty"`
}

type openAPIPath map[string]openAPIOperation

type openAPIOperation struct {
	OperationID        string                     `json:"operationId"`
	Summary            string                     `json:"summary"`
	Tags               []string                   `json:"tags,omitempty"`
	Parameters         []openAPIParameter         `json:"parameters,omitempty"`
	RequestBody        *openAPIRequestBody        `json:"requestBody,omitempty"`
	Responses          map[string]openAPIResponse `json:"responses"`
	Security           []map[string][]string      `json:"security,omitempty"`
	SpiceSymbol        string                     `json:"x-spice-symbol"`
	SpiceModule        string                     `json:"x-spice-module,omitempty"`
	SpiceAuthorization *openAPIAuthorization      `json:"x-spice-authorization,omitempty"`
}

type openAPISecurityScheme struct {
	Type   string `json:"type"`
	Scheme string `json:"scheme"`
}

type openAPIAuthorization struct {
	Owner         string   `json:"owner"`
	Authenticated bool     `json:"authenticated,omitempty"`
	AnyRoles      []string `json:"anyRoles,omitempty"`
	AllRoles      []string `json:"allRoles,omitempty"`
	AllScopes     []string `json:"allScopes,omitempty"`
	Expression    string   `json:"expression,omitempty"`
}

type openAPIParameter struct {
	Name     string        `json:"name"`
	In       string        `json:"in"`
	Required bool          `json:"required"`
	Schema   openAPISchema `json:"schema"`
}

type openAPIRequestBody struct {
	Required bool                    `json:"required"`
	Content  map[string]openAPIMedia `json:"content"`
}

type openAPIResponse struct {
	Description string                  `json:"description"`
	Content     map[string]openAPIMedia `json:"content,omitempty"`
}

type openAPIMedia struct {
	Schema openAPISchema `json:"schema"`
}

type openAPISchema struct {
	Ref                  string                   `json:"$ref,omitempty"`
	Type                 string                   `json:"type,omitempty"`
	Format               string                   `json:"format,omitempty"`
	Description          string                   `json:"description,omitempty"`
	Properties           map[string]openAPISchema `json:"properties,omitempty"`
	Required             []string                 `json:"required,omitempty"`
	Items                *openAPISchema           `json:"items,omitempty"`
	AdditionalProperties *openAPISchema           `json:"additionalProperties,omitempty"`
	AnyOf                []openAPISchema          `json:"anyOf,omitempty"`
}

type openAPISchemaBuilder struct {
	components map[string]openAPISchema
}

func renderOpenAPI(
	model application.Model,
	applicationTarget application.Target,
) ([]byte, error) {
	builder := openAPISchemaBuilder{components: map[string]openAPISchema{
		"SpiceProblem": problemSchema(),
	}}
	document := openAPIDocument{
		OpenAPI: "3.1.0",
		Info: openAPIInfo{
			Title:   applicationTarget.Name + " API",
			Version: GeneratorVersion,
		},
		Paths: make(map[string]openAPIPath),
	}
	operationIDCounts := make(map[string]int)
	protected := false
	for _, item := range model.Controllers() {
		for _, route := range item.Routes() {
			operationIDCounts[item.Name+"_"+route.Name]++
			if _, authorized := route.Authorization(); authorized {
				protected = true
			}
		}
	}
	for _, item := range model.Controllers() {
		for _, route := range item.Routes() {
			pathItem := document.Paths[route.Path]
			if pathItem == nil {
				pathItem = make(openAPIPath)
				document.Paths[route.Path] = pathItem
			}
			operationID := item.Name + "_" + route.Name
			if operationIDCounts[operationID] > 1 {
				operationID += "_" + shortOpenAPIHash(route.SymbolID)
			}
			pathItem[strings.ToLower(route.HTTPMethod)] = buildOpenAPIOperation(
				builder,
				route,
				operationID,
			)
		}
	}
	document.Components.Schemas = builder.components
	if protected {
		document.Components.SecuritySchemes = map[string]openAPISecurityScheme{
			"SpicePrincipal": {
				Type:   "http",
				Scheme: "bearer",
			},
		}
	}
	content, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

func buildOpenAPIOperation(
	builder openAPISchemaBuilder,
	route controller.Route,
	operationID string,
) openAPIOperation {
	operation := openAPIOperation{
		OperationID: operationID,
		Summary:     route.Name,
		Responses:   make(map[string]openAPIResponse),
		SpiceSymbol: route.SymbolID,
		SpiceModule: route.Module,
	}
	if route.Module != "" {
		operation.Tags = []string{route.Module}
	}
	if authorization, protected := route.Authorization(); protected {
		operation.Security = []map[string][]string{
			{"SpicePrincipal": {}},
		}
		operation.SpiceAuthorization = &openAPIAuthorization{
			Owner:         authorization.Module,
			Authenticated: authorization.Authenticated,
			AnyRoles:      authorization.AnyRoles(),
			AllRoles:      authorization.AllRoles(),
			AllScopes:     authorization.AllScopes(),
			Expression:    authorization.Expression(),
		}
		operation.Responses["401"] = problemResponse()
		operation.Responses["403"] = problemResponse()
	}
	if route.Raw {
		operation.Responses["default"] = openAPIResponse{
			Description: "Response owned by raw net/http handler",
		}
		return operation
	}
	formSchema := openAPISchema{
		Type:       "object",
		Properties: make(map[string]openAPISchema),
	}
	for _, binding := range route.Bindings() {
		if binding.Location == controller.Body {
			operation.RequestBody = &openAPIRequestBody{
				Required: true,
				Content: map[string]openAPIMedia{
					"application/json": {Schema: builder.schema(binding.Type)},
				},
			}
			continue
		}
		if binding.Location == controller.Form {
			formSchema.Properties[binding.Name] = parameterSchema(binding)
			if binding.Required {
				formSchema.Required = append(
					formSchema.Required,
					binding.Name,
				)
			}
			continue
		}
		operation.Parameters = append(operation.Parameters, openAPIParameter{
			Name:     binding.Name,
			In:       string(binding.Location),
			Required: binding.Required,
			Schema:   parameterSchema(binding),
		})
	}
	if len(formSchema.Properties) != 0 {
		sort.Strings(formSchema.Required)
		operation.RequestBody = &openAPIRequestBody{
			Required: len(formSchema.Required) != 0,
			Content: map[string]openAPIMedia{
				"application/x-www-form-urlencoded": {
					Schema: formSchema,
				},
			},
		}
	}
	switch {
	case route.NoContent:
		operation.Responses["204"] = openAPIResponse{Description: "No Content"}
	case route.View:
		operation.Responses["200"] = openAPIResponse{
			Description: http.StatusText(http.StatusOK),
			Content: map[string]openAPIMedia{
				"text/html": {Schema: openAPISchema{Type: "string"}},
			},
		}
		operation.Responses["303"] = openAPIResponse{
			Description: http.StatusText(http.StatusSeeOther),
		}
	default:
		operation.Responses["200"] = openAPIResponse{
			Description: http.StatusText(http.StatusOK),
			Content: map[string]openAPIMedia{
				"application/json": {Schema: builder.schema(route.Response)},
			},
		}
	}
	operation.Responses["default"] = problemResponse()
	return operation
}

func parameterSchema(binding controller.Binding) openAPISchema {
	switch binding.Kind {
	case controller.ScalarBoolean:
		return openAPISchema{Type: "boolean"}
	case controller.ScalarInteger:
		return openAPISchema{Type: "integer", Format: integerFormat(binding.Type)}
	case controller.ScalarDuration:
		return openAPISchema{Type: "string", Format: "duration"}
	case controller.ScalarString:
		return openAPISchema{Type: "string"}
	default:
		return openAPISchema{Type: "object"}
	}
}

func integerFormat(value types.Type) string {
	basic, ok := types.Unalias(value).Underlying().(*types.Basic)
	if !ok {
		return "int64"
	}
	kind := basic.Kind()
	if kind == types.Int8 || kind == types.Int16 || kind == types.Int32 ||
		kind == types.Uint8 || kind == types.Uint16 || kind == types.Uint32 {
		return "int32"
	}
	return "int64"
}

func problemResponse() openAPIResponse {
	return openAPIResponse{
		Description: "RFC 9457 problem response",
		Content: map[string]openAPIMedia{
			"application/problem+json": {
				Schema: openAPISchema{Ref: "#/components/schemas/SpiceProblem"},
			},
		},
	}
}

func problemSchema() openAPISchema {
	return openAPISchema{
		Type: "object",
		Properties: map[string]openAPISchema{
			"type":     {Type: "string", Format: "uri-reference"},
			"title":    {Type: "string"},
			"status":   {Type: "integer", Format: "int32"},
			"detail":   {Type: "string"},
			"instance": {Type: "string", Format: "uri-reference"},
		},
		Required: []string{"type", "title", "status"},
	}
}

func (builder openAPISchemaBuilder) schema(value types.Type) openAPISchema {
	value = types.Unalias(value)
	switch current := value.(type) {
	case *types.Pointer:
		item := builder.schema(current.Elem())
		return openAPISchema{AnyOf: []openAPISchema{item, {Type: "null"}}}
	case *types.Named:
		return builder.namedSchema(current)
	case *types.Basic:
		return basicSchema(current)
	case *types.Array:
		item := builder.schema(current.Elem())
		return openAPISchema{Type: "array", Items: &item}
	case *types.Slice:
		item := builder.schema(current.Elem())
		return openAPISchema{Type: "array", Items: &item}
	case *types.Map:
		item := builder.schema(current.Elem())
		return openAPISchema{Type: "object", AdditionalProperties: &item}
	case *types.Struct:
		return builder.structSchema(current)
	default:
		return openAPISchema{
			Type:        "object",
			Description: "Schema for " + types.TypeString(value, packageQualifier),
		}
	}
}

func (builder openAPISchemaBuilder) namedSchema(value *types.Named) openAPISchema {
	if namedType(value, "time", "Time") {
		return openAPISchema{Type: "string", Format: "date-time"}
	}
	if namedType(value, "time", "Duration") {
		return openAPISchema{Type: "string", Format: "duration"}
	}
	name := openAPIComponentName(value)
	if _, found := builder.components[name]; !found {
		builder.components[name] = openAPISchema{Type: "object"}
		builder.components[name] = builder.schema(value.Underlying())
	}
	return openAPISchema{Ref: "#/components/schemas/" + name}
}

func (builder openAPISchemaBuilder) structSchema(value *types.Struct) openAPISchema {
	schema := openAPISchema{
		Type:       "object",
		Properties: make(map[string]openAPISchema),
	}
	for index := 0; index < value.NumFields(); index++ {
		field := value.Field(index)
		if !field.Exported() {
			continue
		}
		name, included, required := openAPIJSONField(field, value.Tag(index))
		if !included {
			continue
		}
		schema.Properties[name] = builder.schema(field.Type())
		if required {
			schema.Required = append(schema.Required, name)
		}
	}
	sort.Strings(schema.Required)
	return schema
}

func basicSchema(value *types.Basic) openAPISchema {
	switch {
	case value.Info()&types.IsBoolean != 0:
		return openAPISchema{Type: "boolean"}
	case value.Info()&types.IsInteger != 0:
		return openAPISchema{Type: "integer", Format: integerFormat(value)}
	case value.Info()&types.IsFloat != 0:
		return openAPISchema{Type: "number", Format: "double"}
	case value.Info()&types.IsString != 0:
		return openAPISchema{Type: "string"}
	default:
		return openAPISchema{Type: "object"}
	}
}

func openAPIJSONField(field *types.Var, tag string) (string, bool, bool) {
	value, present := reflect.StructTag(tag).Lookup("json")
	if !present {
		return field.Name(), true, true
	}
	parts := strings.Split(value, ",")
	if parts[0] == "-" {
		return "", false, false
	}
	name := parts[0]
	if name == "" {
		name = field.Name()
	}
	required := true
	for _, option := range parts[1:] {
		if option == "omitempty" || option == "omitzero" {
			required = false
		}
	}
	return name, true, required
}

func openAPIComponentName(value *types.Named) string {
	label := types.TypeString(value, packageQualifier)
	name := openAPINamePart.ReplaceAllString(value.Obj().Name(), "_")
	return name + "_" + shortOpenAPIHash(label)
}

func shortOpenAPIHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:5])
}

func packageQualifier(pkg *types.Package) string {
	if pkg == nil {
		return ""
	}
	return pkg.Path()
}

func namedType(value *types.Named, packagePath, name string) bool {
	return value.Obj() != nil && value.Obj().Pkg() != nil &&
		value.Obj().Pkg().Path() == packagePath && value.Obj().Name() == name
}
