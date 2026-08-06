package cli

import (
	"fmt"
	"io"
	"slices"
)

// Command dispatches the generated collection of explicit production command
// handlers. It owns no reflection, mutable global registry, or hidden stream.
type Command struct {
	handlers       map[string]Handler
	defaultHandler Handler
}

// NewCommand constructs an isolated Spice command bean.
func NewCommand(handlers []Handler) (*Command, error) {
	if len(handlers) == 0 {
		return nil, fmt.Errorf("construct Spice command: no handlers")
	}
	command := &Command{
		handlers: make(map[string]Handler),
	}
	for index, handler := range handlers {
		if handler == nil {
			return nil, fmt.Errorf(
				"construct Spice command: handler %d is nil",
				index,
			)
		}
		names := handler.Names()
		if len(names) == 0 {
			return nil, fmt.Errorf(
				"construct Spice command: handler %d has no names",
				index,
			)
		}
		for _, name := range names {
			if _, duplicate := command.handlers[name]; duplicate {
				return nil, fmt.Errorf(
					"construct Spice command: duplicate handler name %q",
					name,
				)
			}
			command.handlers[name] = handler
			if name == "help" {
				command.defaultHandler = handler
			}
		}
	}
	if command.defaultHandler == nil {
		return nil, fmt.Errorf(
			"construct Spice command: help handler is missing",
		)
	}
	return command, nil
}

// Run executes one Spice command and returns its process exit code.
func (command *Command) Run(
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if command == nil {
		if err := writef(stderr, "Spice command is unavailable.\n"); err != nil {
			return 1
		}
		return 1
	}
	handler := command.defaultHandler
	handlerArguments := arguments
	if len(arguments) != 0 {
		var found bool
		handler, found = command.handlers[arguments[0]]
		if !found {
			return unknownCommand(arguments[0], stderr)
		}
		handlerArguments = arguments[1:]
	}
	return handler.Run(Invocation{
		Arguments: handlerArguments,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
	})
}

// Names returns every accepted command name in deterministic order.
func (command *Command) Names() []string {
	if command == nil {
		return nil
	}
	names := make([]string, 0, len(command.handlers))
	for name := range command.handlers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
