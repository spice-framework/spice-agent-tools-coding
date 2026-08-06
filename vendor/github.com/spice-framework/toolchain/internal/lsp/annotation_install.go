package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spice-framework/toolchain/compiler/annotationinstall"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
)

const (
	annotationToolPreviewCommand = "spice.annotationTool.previewInstall"
	annotationToolApplyCommand   = "spice.annotationTool.applyInstall"
	annotationToolCommandTimeout = 2 * time.Minute
)

type executeCommandParams struct {
	Command   string            `json:"command"`
	Arguments []json.RawMessage `json:"arguments,omitempty"`
}

type annotationToolCommandArguments struct {
	Root    string `json:"root"`
	Tool    string `json:"tool"`
	Version string `json:"version,omitempty"`
	Token   string `json:"token,omitempty"`
}

type annotationToolPreviewer func(
	context.Context,
	string,
	string,
	string,
	[]string,
) (annotationinstall.Preview, error)

type annotationToolApplier func(
	context.Context,
	annotationinstall.Preview,
) error

func (server *Server) annotationToolCodeActions(
	source document,
	requestRange protocolRange,
	metadata metadataView,
) []protocolCodeAction {
	occurrence, found := occurrenceOverlappingRange(
		source.content,
		requestRange,
	)
	if !found {
		return nil
	}
	definition, found := definitionForOccurrence(
		source,
		metadata,
		occurrence,
	)
	if !found ||
		definition.Implementation.Tool == "" ||
		!definition.Implementation.AuthorizationKnown ||
		definition.Implementation.Authorized {
		return nil
	}
	arguments := annotationToolCommandArguments{
		Root:    source.root,
		Tool:    definition.Implementation.Tool,
		Version: definition.Provenance.Version,
	}
	key := annotationToolPreviewKey(arguments.Root, arguments.Tool)
	server.mu.Lock()
	token := server.toolPreviewByKey[key]
	preview, previewed := server.toolPreviews[token]
	server.mu.Unlock()
	var result []protocolCodeAction
	if previewed &&
		pathKey(preview.Root()) == pathKey(arguments.Root) &&
		preview.Tool() == arguments.Tool &&
		preview.Version() == arguments.Version {
		applyArguments := arguments
		applyArguments.Token = token
		title := "Apply previewed " + preview.Command()
		result = append(result, protocolCodeAction{
			Title:       title,
			Kind:        "quickfix",
			IsPreferred: true,
			Command: &protocolCommand{
				Title:     title,
				Command:   annotationToolApplyCommand,
				Arguments: []any{applyArguments},
			},
		})
	}
	selector := arguments.Tool
	if arguments.Version != "" {
		selector += "@" + arguments.Version
	}
	title := "Preview go get -tool " + selector
	result = append(result, protocolCodeAction{
		Title:       title,
		Kind:        "quickfix",
		IsPreferred: !previewed,
		Command: &protocolCommand{
			Title:     title,
			Command:   annotationToolPreviewCommand,
			Arguments: []any{arguments},
		},
	})
	return result
}

func occurrenceOverlappingRange(
	content []byte,
	requestRange protocolRange,
) (annotationOccurrence, bool) {
	for _, occurrence := range annotationOccurrences(content) {
		occurrenceRange := protocolRangeAtOffsets(
			content,
			occurrence.start,
			occurrence.end,
		)
		if rangesOverlap(occurrenceRange, requestRange) {
			return occurrence, true
		}
	}
	return annotationOccurrence{}, false
}

func (server *Server) startExecuteCommand(message rpcMessage) error {
	key := string(message.ID)
	ctx, cancel := context.WithCancel(context.Background())
	server.mu.Lock()
	if _, active := server.requests[key]; active {
		server.mu.Unlock()
		cancel()
		return server.writer.failure(
			message.ID,
			invalidRequestCode,
			"an LSP request with this ID is already active",
		)
	}
	server.requests[key] = cancel
	server.requestWait.Add(1)
	server.mu.Unlock()
	go func() {
		defer server.requestWait.Done()
		err := server.executeCommandContext(ctx, message)
		cancel()
		server.mu.Lock()
		delete(server.requests, key)
		if err != nil {
			server.asyncErr = errors.Join(server.asyncErr, err)
		}
		runCancel := server.cancel
		server.mu.Unlock()
		if err != nil && runCancel != nil {
			runCancel()
		}
	}()
	return nil
}

func (server *Server) executeCommand(message rpcMessage) error {
	return server.executeCommandContext(context.Background(), message)
}

func (server *Server) executeCommandContext(
	parent context.Context,
	message rpcMessage,
) error {
	if !message.request() {
		return nil
	}
	var params executeCommandParams
	if err := decodeParams(message.Params, &params); err != nil {
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			err.Error(),
		)
	}
	arguments, err := decodeAnnotationToolCommandArguments(params.Arguments)
	if err != nil {
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			err.Error(),
		)
	}
	ctx, cancel := context.WithTimeout(
		parent,
		annotationToolCommandTimeout,
	)
	defer cancel()
	switch params.Command {
	case annotationToolPreviewCommand:
		return server.previewAnnotationTool(ctx, message.ID, arguments)
	case annotationToolApplyCommand:
		return server.applyAnnotationTool(ctx, message.ID, arguments)
	default:
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			fmt.Sprintf(
				"unsupported Spice command %q",
				params.Command,
			),
		)
	}
}

func decodeAnnotationToolCommandArguments(
	values []json.RawMessage,
) (annotationToolCommandArguments, error) {
	if len(values) != 1 {
		return annotationToolCommandArguments{}, errors.New(
			"annotation tool command requires exactly one argument",
		)
	}
	var result annotationToolCommandArguments
	if err := json.Unmarshal(values[0], &result); err != nil {
		return annotationToolCommandArguments{}, fmt.Errorf(
			"decode annotation tool command argument: %w",
			err,
		)
	}
	if result.Root == "" || result.Tool == "" {
		return annotationToolCommandArguments{}, errors.New(
			"annotation tool command requires workspace root and tool path",
		)
	}
	return result, nil
}

func (server *Server) previewAnnotationTool(
	ctx context.Context,
	id json.RawMessage,
	arguments annotationToolCommandArguments,
) error {
	compiler, found := server.annotationToolWorkspace(
		arguments.Root,
	)
	if !found {
		return server.writer.failure(
			id,
			invalidParamsCode,
			"annotation tool preview root is not an active workspace",
		)
	}
	catalog, err := compiler.AnnotationCatalog(ctx, arguments.Root)
	if err != nil {
		return server.annotationToolCommandFailure(id, err)
	}
	if !unauthorizedCatalogTool(catalog, arguments) {
		return server.writer.failure(
			id,
			invalidParamsCode,
			"annotation tool preview no longer matches an unauthorized catalog descriptor",
		)
	}
	previewTool := server.previewTool
	if previewTool == nil {
		previewTool = annotationinstall.PreviewTool
	}
	preview, err := previewTool(
		ctx,
		arguments.Root,
		arguments.Tool,
		arguments.Version,
		nil,
	)
	if err != nil {
		return server.annotationToolCommandFailure(id, err)
	}
	key := annotationToolPreviewKey(arguments.Root, arguments.Tool)
	server.mu.Lock()
	if previous := server.toolPreviewByKey[key]; previous != "" {
		delete(server.toolPreviews, previous)
	}
	server.toolPreviews[preview.Token()] = preview
	server.toolPreviewByKey[key] = preview.Token()
	server.mu.Unlock()
	message := strings.Join([]string{
		"Spice prepared an annotation-tool installation preview. No files were changed.",
		"",
		"Command:",
		preview.Command(),
		"",
		"Exact go.mod/go.sum diff:",
		preview.Diff(),
		"",
		"Run the “Apply previewed” quick fix to confirm this exact change.",
	}, "\n")
	if err := server.writer.notification(
		"window/showMessage",
		map[string]any{"type": 3, "message": message},
	); err != nil {
		return err
	}
	return server.writer.response(id, map[string]any{
		"status":  "previewed",
		"token":   preview.Token(),
		"command": preview.Command(),
		"diff":    preview.Diff(),
	})
}

func (server *Server) applyAnnotationTool(
	ctx context.Context,
	id json.RawMessage,
	arguments annotationToolCommandArguments,
) error {
	if arguments.Token == "" {
		return server.writer.failure(
			id,
			invalidParamsCode,
			"annotation tool apply requires a preview token",
		)
	}
	compiler, found := server.annotationToolWorkspace(
		arguments.Root,
	)
	if !found {
		return server.writer.failure(
			id,
			invalidParamsCode,
			"annotation tool apply root is not an active workspace",
		)
	}
	server.mu.Lock()
	preview, found := server.toolPreviews[arguments.Token]
	server.mu.Unlock()
	if !found ||
		pathKey(preview.Root()) != pathKey(arguments.Root) ||
		preview.Tool() != arguments.Tool ||
		preview.Version() != arguments.Version {
		return server.writer.failure(
			id,
			invalidParamsCode,
			"annotation tool apply does not match an active preview",
		)
	}
	applyTool := server.applyTool
	if applyTool == nil {
		applyTool = annotationinstall.Apply
	}
	if err := applyTool(ctx, preview); err != nil {
		return server.annotationToolCommandFailure(id, err)
	}
	if err := compiler.InvalidateAnnotationCatalog(arguments.Root); err != nil {
		return server.writer.failure(id, internalErrorCode, err.Error())
	}
	key := annotationToolPreviewKey(arguments.Root, arguments.Tool)
	server.mu.Lock()
	delete(server.toolPreviews, arguments.Token)
	delete(server.toolPreviewByKey, key)
	if workspace := server.workspaces[pathKey(arguments.Root)]; server.run &&
		workspace != nil {
		server.scheduleWorkspaceLocked(pathKey(arguments.Root), workspace)
	}
	server.mu.Unlock()
	if err := server.writer.notification(
		"window/showMessage",
		map[string]any{
			"type":    3,
			"message": "Applied " + preview.Command() + ".",
		},
	); err != nil {
		return err
	}
	return server.writer.response(id, map[string]any{
		"status":  "applied",
		"command": preview.Command(),
	})
}

func (server *Server) annotationToolCommandFailure(
	id json.RawMessage,
	err error,
) error {
	code := internalErrorCode
	if errors.Is(err, context.Canceled) {
		code = requestCancelledCode
	}
	return server.writer.failure(id, code, err.Error())
}

func (server *Server) annotationToolWorkspace(
	root string,
) (*compilerservice.Service, bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	workspace := server.workspaces[pathKey(root)]
	if workspace == nil ||
		workspace.service == nil ||
		pathKey(workspace.root) != pathKey(root) {
		return nil, false
	}
	return workspace.service, true
}

func unauthorizedCatalogTool(
	catalog []compilerservice.AnnotationDefinition,
	arguments annotationToolCommandArguments,
) bool {
	for _, definition := range catalog {
		if definition.Implementation.Tool == arguments.Tool &&
			definition.Provenance.Version == arguments.Version &&
			definition.Implementation.AuthorizationKnown &&
			!definition.Implementation.Authorized {
			return true
		}
	}
	return false
}

func annotationToolPreviewKey(root, tool string) string {
	return pathKey(root) + "\x00" + tool
}
