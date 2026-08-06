package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Invocation is one isolated command execution. The process entrypoint owns
// all streams; handlers never read or replace package-global standard streams.
type Invocation struct {
	Arguments []string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
}

// Handler is one named CLI operation. Production Spice injects every
// implementation as an explicit interface bean and generated Go assembles the
// ordered collection.
type Handler interface {
	Names() []string
	Run(Invocation) int
}

type commandHandler struct {
	runtime *Runtime
	names   []string
	run     func(*Runtime, Invocation) int
}

func newCommandHandler(
	runtime *Runtime,
	names []string,
	run func(*Runtime, Invocation) int,
) (Handler, error) {
	if runtime == nil {
		return nil, errors.New("spice CLI runtime is nil")
	}
	if run == nil {
		return nil, errors.New("spice CLI handler function is nil")
	}
	normalized := append([]string(nil), names...)
	for index, name := range normalized {
		if name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf(
				"spice CLI handler name %d is invalid: %q",
				index,
				name,
			)
		}
	}
	slices.Sort(normalized)
	if duplicate := firstDuplicate(normalized); duplicate != "" {
		return nil, fmt.Errorf(
			"spice CLI handler repeats name %q",
			duplicate,
		)
	}
	return &commandHandler{
		runtime: runtime,
		names:   normalized,
		run:     run,
	}, nil
}

func newLoaderCommandHandler(
	runtime *Runtime,
	names []string,
	run func(*Runtime, Invocation) int,
) (Handler, error) {
	if err := runtime.validateLoader(); err != nil {
		return nil, err
	}
	return newCommandHandler(runtime, names, run)
}

func newBuildCommandHandler(
	runtime *Runtime,
	names []string,
	run func(*Runtime, Invocation) int,
) (Handler, error) {
	if err := runtime.validateLoader(); err != nil {
		return nil, err
	}
	if err := runtime.validateBuilder(); err != nil {
		return nil, err
	}
	return newCommandHandler(runtime, names, run)
}

func newTestCommandHandler(
	runtime *Runtime,
	names []string,
	run func(*Runtime, Invocation) int,
) (Handler, error) {
	if err := runtime.validateLoader(); err != nil {
		return nil, err
	}
	if err := runtime.validateTester(); err != nil {
		return nil, err
	}
	return newCommandHandler(runtime, names, run)
}

func firstDuplicate(values []string) string {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return values[index]
		}
	}
	return ""
}

func (handler *commandHandler) Names() []string {
	if handler == nil {
		return nil
	}
	return append([]string(nil), handler.names...)
}

func (handler *commandHandler) Run(invocation Invocation) int {
	if handler == nil || handler.runtime == nil || handler.run == nil {
		if err := writef(
			invocation.Stderr,
			"Spice CLI handler is unavailable.\n",
		); err != nil {
			return 1
		}
		return 1
	}
	invocation.Arguments = append([]string(nil), invocation.Arguments...)
	return handler.run(handler.runtime, invocation)
}
