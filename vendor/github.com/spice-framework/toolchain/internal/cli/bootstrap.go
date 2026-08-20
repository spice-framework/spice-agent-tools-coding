package cli

import "fmt"

type handlerFactory func(*Runtime) (Handler, error)

// newBootstrapCommand is the ordinary-Go stage-zero assembly. Production
// cmd/spice obtains the same factories as generated interface beans.
func newBootstrapCommand(runtime *Runtime) (*Command, error) {
	factories := []handlerFactory{
		NewHelpHandler,
		NewVersionHandler,
		NewInitHandler,
		NewScaffoldHandler,
		NewAddHandler,
		NewVerifyHandler,
		NewAnnotationsHandler,
		NewModulesHandler,
		NewBeansHandler,
		NewGeneratedHandler,
		NewTestHandler,
		NewGenerateHandler,
		NewBuildHandler,
		NewRunHandler,
		NewDevHandler,
		NewLSPHandler,
	}
	handlers := make([]Handler, 0, len(factories))
	for index, factory := range factories {
		handler, err := factory(runtime)
		if err != nil {
			return nil, fmt.Errorf(
				"construct bootstrap command handler %d: %w",
				index,
				err,
			)
		}
		handlers = append(handlers, handler)
	}
	command, err := NewCommand(handlers)
	if err != nil {
		return nil, fmt.Errorf("construct bootstrap command: %w", err)
	}
	return command, nil
}
