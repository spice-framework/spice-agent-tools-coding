package modulith

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Format identifies one portable module-documentation representation.
type Format string

const (
	// FormatJSON emits the complete machine-readable module canvas.
	FormatJSON Format = "json"
	// FormatMermaid emits a Mermaid flowchart.
	FormatMermaid Format = "mermaid"
	// FormatPlantUML emits a PlantUML component diagram.
	FormatPlantUML Format = "plantuml"
)

const documentSchema = "spice.modules/v1"

type moduleDocument struct {
	ID                   string                   `json:"id"`
	RootPackage          string                   `json:"root_package"`
	Packages             []string                 `json:"packages"`
	DefaultAPI           string                   `json:"default_api"`
	NamedInterfaces      []namedInterfaceDocument `json:"named_interfaces"`
	AllowedDependencies  []string                 `json:"allowed_dependencies"`
	ObservedDependencies []string                 `json:"observed_dependencies"`
}

type namedInterfaceDocument struct {
	Name        string `json:"name"`
	PackagePath string `json:"package"`
}

type edgeDocument struct {
	FromModule  string `json:"from_module"`
	ToModule    string `json:"to_module"`
	API         string `json:"api"`
	FromPackage string `json:"from_package"`
	ToPackage   string `json:"to_package"`
}

type cycleDocument struct {
	Members []string `json:"members"`
	Path    []string `json:"path"`
}

type moduleGraphDocument struct {
	Schema             string           `json:"schema"`
	Focus              string           `json:"focus,omitempty"`
	DependencyOrder    []string         `json:"dependency_order,omitempty"`
	Modules            []moduleDocument `json:"modules"`
	Edges              []edgeDocument   `json:"edges"`
	Cycles             []cycleDocument  `json:"cycles"`
	UnassignedPackages []string         `json:"unassigned_packages"`
}

type diagramEdge struct {
	from, to string
	apis     []string
}

// Render emits a deterministic, portable representation of model.
func Render(model Model, format Format) ([]byte, error) {
	switch format {
	case FormatJSON:
		return renderJSON(model)
	case FormatMermaid:
		return renderMermaid(model), nil
	case FormatPlantUML:
		return renderPlantUML(model), nil
	default:
		return nil, fmt.Errorf(
			"unsupported module format %q; expected json, mermaid, or plantuml",
			format,
		)
	}
}

func renderJSON(model Model) ([]byte, error) {
	document := moduleGraphDocument{
		Schema:             documentSchema,
		Focus:              model.FocusID(),
		DependencyOrder:    model.DependencyOrder(),
		Modules:            make([]moduleDocument, 0),
		Edges:              make([]edgeDocument, 0),
		Cycles:             make([]cycleDocument, 0),
		UnassignedPackages: make([]string, 0),
	}
	observed := observedDependencies(model.Edges())
	for _, module := range model.Modules() {
		canvas := moduleDocument{
			ID:                   module.ID,
			RootPackage:          module.RootPackage,
			DefaultAPI:           module.RootPackage,
			Packages:             make([]string, 0),
			NamedInterfaces:      make([]namedInterfaceDocument, 0),
			AllowedDependencies:  make([]string, 0),
			ObservedDependencies: append([]string(nil), observed[module.ID]...),
		}
		if canvas.ObservedDependencies == nil {
			canvas.ObservedDependencies = make([]string, 0)
		}
		for _, pkg := range module.Packages() {
			canvas.Packages = append(canvas.Packages, pkg.Path)
		}
		for _, item := range module.NamedInterfaces() {
			canvas.NamedInterfaces = append(canvas.NamedInterfaces, namedInterfaceDocument{
				Name:        item.Name,
				PackagePath: item.PackagePath,
			})
		}
		for _, dependency := range module.AllowedDependencies() {
			canvas.AllowedDependencies = append(canvas.AllowedDependencies, dependency.String())
		}
		document.Modules = append(document.Modules, canvas)
	}
	for _, edge := range model.Edges() {
		api := "default"
		if edge.API != "" {
			api = edge.API
		}
		document.Edges = append(document.Edges, edgeDocument{
			FromModule:  edge.FromModule,
			ToModule:    edge.ToModule,
			API:         api,
			FromPackage: edge.FromPackage,
			ToPackage:   edge.ToPackage,
		})
	}
	for _, cycle := range model.Cycles() {
		document.Cycles = append(document.Cycles, cycleDocument{
			Members: append([]string(nil), cycle.Members...),
			Path:    append([]string(nil), cycle.Path...),
		})
	}
	for _, pkg := range model.UnassignedPackages() {
		document.UnassignedPackages = append(document.UnassignedPackages, pkg.Path)
	}
	content, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode module graph JSON: %w", err)
	}
	return append(content, '\n'), nil
}

func observedDependencies(edges []Edge) map[string][]string {
	sets := make(map[string]map[string]struct{})
	for _, edge := range edges {
		if sets[edge.FromModule] == nil {
			sets[edge.FromModule] = make(map[string]struct{})
		}
		sets[edge.FromModule][dependencyIdentity(edge.ToModule, edge.API)] = struct{}{}
	}
	result := make(map[string][]string, len(sets))
	for moduleID, dependencies := range sets {
		for dependency := range dependencies {
			result[moduleID] = append(result[moduleID], dependency)
		}
		sort.Strings(result[moduleID])
	}
	return result
}

func renderMermaid(model Model) []byte {
	var output bytes.Buffer
	output.WriteString("%% Generated by Spice. DO NOT EDIT.\n")
	output.WriteString("flowchart LR\n")
	aliases := moduleAliases(model.Modules())
	for _, module := range model.Modules() {
		fmt.Fprintf(&output, "  %s[%s]\n", aliases[module.ID], strconv.Quote(module.ID))
	}
	unassigned := model.UnassignedPackages()
	for index, pkg := range unassigned {
		fmt.Fprintf(
			&output,
			"  u%d[%s]:::unassigned\n",
			index,
			strconv.Quote("unassigned: "+pkg.Path),
		)
	}
	for _, edge := range diagramEdges(model.Edges()) {
		fmt.Fprintf(
			&output,
			"  %s -->|%s| %s\n",
			aliases[edge.from],
			strings.Join(edge.apis, ", "),
			aliases[edge.to],
		)
	}
	if len(unassigned) != 0 {
		output.WriteString("  classDef unassigned fill:#f3f4f6,stroke:#6b7280,stroke-dasharray: 5 5\n")
	}
	if focus := model.FocusID(); focus != "" {
		fmt.Fprintf(&output, "  class %s focus\n", aliases[focus])
		output.WriteString("  classDef focus fill:#dbeafe,stroke:#2563eb,stroke-width:2px\n")
	}
	return output.Bytes()
}

func renderPlantUML(model Model) []byte {
	var output bytes.Buffer
	output.WriteString("' Generated by Spice. DO NOT EDIT.\n")
	output.WriteString("@startuml\n")
	output.WriteString("left to right direction\n")
	aliases := moduleAliases(model.Modules())
	for _, module := range model.Modules() {
		color := ""
		if module.ID == model.FocusID() {
			color = " #LightBlue"
		}
		fmt.Fprintf(
			&output,
			"component %s as %s%s\n",
			strconv.Quote(module.ID),
			aliases[module.ID],
			color,
		)
	}
	for index, pkg := range model.UnassignedPackages() {
		fmt.Fprintf(
			&output,
			"component %s as U%d #LightGray\n",
			strconv.Quote("unassigned: "+pkg.Path),
			index,
		)
	}
	for _, edge := range diagramEdges(model.Edges()) {
		fmt.Fprintf(
			&output,
			"%s --> %s : %s\n",
			aliases[edge.from],
			aliases[edge.to],
			strings.Join(edge.apis, ", "),
		)
	}
	output.WriteString("@enduml\n")
	return output.Bytes()
}

func moduleAliases(modules []Module) map[string]string {
	result := make(map[string]string, len(modules))
	for index, module := range modules {
		result[module.ID] = "M" + strconv.Itoa(index)
	}
	return result
}

func diagramEdges(edges []Edge) []diagramEdge {
	apisByPair := make(map[string]map[string]struct{})
	for _, edge := range edges {
		key := edge.FromModule + "\x00" + edge.ToModule
		if apisByPair[key] == nil {
			apisByPair[key] = make(map[string]struct{})
		}
		api := "default"
		if edge.API != "" {
			api = edge.API
		}
		apisByPair[key][api] = struct{}{}
	}
	keys := make([]string, 0, len(apisByPair))
	for key := range apisByPair {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]diagramEdge, 0, len(keys))
	for _, key := range keys {
		from, to, _ := strings.Cut(key, "\x00")
		item := diagramEdge{from: from, to: to}
		for api := range apisByPair[key] {
			item.apis = append(item.apis, api)
		}
		sort.Strings(item.apis)
		result = append(result, item)
	}
	return result
}
