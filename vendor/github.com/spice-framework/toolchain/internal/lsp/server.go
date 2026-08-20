// @import { Module } from "github.com/spice-framework/spice/annotation/modulith"

// Package lsp implements Spice's editor-neutral Language Server Protocol
// transport over standard JSON-RPC streams.
//
// @Module(allowedDependencies=["github.com/spice-framework/toolchain/compiler::annotationinstall", "github.com/spice-framework/toolchain/compiler::diagnostic", "github.com/spice-framework/toolchain/compiler::parser", "github.com/spice-framework/toolchain/compiler::service", "github.com/spice-framework/toolchain/compiler::style"])
package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spice-framework/toolchain/compiler/annotationinstall"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
	compilerstyle "github.com/spice-framework/toolchain/compiler/style"
)

const (
	defaultAnalysisDelay = 150 * time.Millisecond
	defaultDocumentBytes = 16 << 20

	invalidRequestCode       = -32600
	methodNotFoundCode       = -32601
	invalidParamsCode        = -32602
	internalErrorCode        = -32603
	serverNotInitializedCode = -32002
	requestCancelledCode     = -32800
)

// ErrExitWithoutShutdown reports a protocol exit notification that was not
// preceded by the required shutdown request.
var ErrExitWithoutShutdown = errors.New(
	"LSP client exited without a successful shutdown request",
)

// ServiceFactory creates one isolated compiler service for a workspace root.
type ServiceFactory func(string) (*compilerservice.Service, error)

// Config defines bounded language-server dependencies and behavior.
type Config struct {
	NewService       ServiceFactory
	AnalysisDelay    time.Duration
	MaxDocumentBytes int
}

// Server owns one protocol connection and isolated workspace state.
type Server struct {
	config serverConfig

	mu               sync.Mutex
	run              bool
	initialized      bool
	shutdown         bool
	closing          bool
	done             <-chan struct{}
	cancel           context.CancelFunc
	writer           *rpcWriter
	target           string
	patterns         []string
	profile          compilerstyle.Profile
	workspaces       map[string]*workspace
	documents        map[string]*document
	requests         map[string]context.CancelFunc
	toolPreviews     map[string]annotationinstall.Preview
	toolPreviewByKey map[string]string
	analysisWait     sync.WaitGroup
	requestWait      sync.WaitGroup
	asyncErr         error
	previewTool      annotationToolPreviewer
	applyTool        annotationToolApplier
}

type serverConfig struct {
	newService       ServiceFactory
	analysisDelay    time.Duration
	maxDocumentBytes int
}

type document struct {
	uri      string
	path     string
	language string
	version  int
	content  []byte
	root     string
}

type workspace struct {
	root      string
	uri       string
	service   *compilerservice.Service
	analysis  sync.WaitGroup
	sequence  uint64
	timer     *time.Timer
	cancel    context.CancelFunc
	latest    compilerservice.Result
	lastGood  compilerservice.Result
	hasLatest bool
	hasGood   bool
	target    string
	patterns  []string
	profile   compilerstyle.Profile
	published map[string]struct{}
	documents map[string]struct{}
}

// New creates one reusable-until-run language server.
func New(config Config) (*Server, error) {
	if config.NewService == nil {
		return nil, errors.New("LSP compiler service factory must not be nil")
	}
	if config.AnalysisDelay == 0 {
		config.AnalysisDelay = defaultAnalysisDelay
	}
	if config.AnalysisDelay < 0 {
		return nil, errors.New("LSP analysis delay must not be negative")
	}
	if config.MaxDocumentBytes == 0 {
		config.MaxDocumentBytes = defaultDocumentBytes
	}
	if config.MaxDocumentBytes <= 0 {
		return nil, errors.New("LSP document byte limit must be positive")
	}
	return &Server{
		config: serverConfig{
			newService:       config.NewService,
			analysisDelay:    config.AnalysisDelay,
			maxDocumentBytes: config.MaxDocumentBytes,
		},
		workspaces: make(map[string]*workspace),
		documents:  make(map[string]*document),
		requests:   make(map[string]context.CancelFunc),
		toolPreviews: make(
			map[string]annotationinstall.Preview,
		),
		toolPreviewByKey: make(map[string]string),
		previewTool:      annotationinstall.PreviewTool,
		applyTool:        annotationinstall.Apply,
	}, nil
}

// Run serves one JSON-RPC connection until the client sends exit, the context
// ends, the input closes, or a transport error occurs.
func (server *Server) Run(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
) (runErr error) {
	if ctx == nil {
		return errors.New("LSP context must not be nil")
	}
	if input == nil || output == nil {
		return errors.New("LSP input and output must not be nil")
	}
	server.mu.Lock()
	if server.run {
		server.mu.Unlock()
		return errors.New("LSP server may be run only once")
	}
	server.run = true
	runCtx, cancel := context.WithCancel(ctx)
	server.done = runCtx.Done()
	server.cancel = cancel
	server.writer = newRPCWriter(output)
	server.mu.Unlock()

	defer func() {
		runErr = errors.Join(runErr, server.stop())
	}()
	reader := newRPCReader(input)
	for {
		message, err := readRPCContext(runCtx, reader, input)
		if err != nil {
			if clientError, ok := errors.AsType[*rpcClientError](err); ok {
				if writeErr := server.writer.failure(
					clientError.id,
					clientError.code,
					clientError.message,
				); writeErr != nil {
					return writeErr
				}
				continue
			}
			if errors.Is(err, io.EOF) && server.shutdownState() {
				return nil
			}
			return err
		}
		exit, handleErr := server.handle(message)
		if handleErr != nil {
			return handleErr
		}
		if exit {
			if server.shutdownState() {
				return nil
			}
			return ErrExitWithoutShutdown
		}
	}
}

type rpcReadResult struct {
	message rpcMessage
	err     error
}

func readRPCContext(
	ctx context.Context,
	reader *rpcReader,
	input io.Reader,
) (rpcMessage, error) {
	result := make(chan rpcReadResult, 1)
	go func() {
		message, err := reader.read()
		result <- rpcReadResult{message: message, err: err}
	}()
	select {
	case <-ctx.Done():
		if closer, ok := input.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				return rpcMessage{}, errors.Join(ctx.Err(), err)
			}
		}
		return rpcMessage{}, ctx.Err()
	case received := <-result:
		return received.message, received.err
	}
}

func (server *Server) handle(message rpcMessage) (bool, error) {
	switch message.Method {
	case "initialize":
		return false, server.initialize(message)
	case "exit":
		return true, nil
	case "$/cancelRequest":
		server.cancelRequest(message.Params)
		return false, nil
	}
	return server.handleInitialized(message)
}

func (server *Server) handleInitialized(
	message rpcMessage,
) (bool, error) {
	server.mu.Lock()
	initialized := server.initialized
	shutdown := server.shutdown
	server.mu.Unlock()
	if !initialized {
		if message.request() {
			return false, server.writer.failure(
				message.ID,
				serverNotInitializedCode,
				"Spice language server is not initialized",
			)
		}
		return false, nil
	}
	if shutdown {
		if message.request() {
			return false, server.writer.failure(
				message.ID,
				invalidRequestCode,
				"Spice language server has shut down",
			)
		}
		return false, nil
	}

	if !message.request() {
		return false, server.handleNotification(message)
	}
	if message.Method == "workspace/executeCommand" {
		return false, server.startExecuteCommand(message)
	}
	return false, server.handleRequest(message)
}

func (server *Server) handleNotification(message rpcMessage) error {
	switch message.Method {
	case "initialized":
		return nil
	case "textDocument/didOpen":
		server.didOpen(message.Params)
		return nil
	case "textDocument/didChange":
		server.didChange(message.Params)
		return nil
	case "textDocument/didSave":
		server.didSave(message.Params)
		return nil
	case "textDocument/didClose":
		return server.didClose(message.Params)
	case "workspace/didChangeWorkspaceFolders":
		server.didChangeWorkspaceFolders(message.Params)
		return nil
	case "workspace/didChangeConfiguration":
		server.refreshAll(message.Params)
		return nil
	default:
		return nil
	}
}

func (server *Server) handleRequest(message rpcMessage) error {
	switch message.Method {
	case "shutdown":
		return server.handleShutdown(message)
	case "textDocument/completion":
		return server.completion(message)
	case "textDocument/hover":
		return server.hover(message)
	case "textDocument/signatureHelp":
		return server.signatureHelp(message)
	case "textDocument/definition":
		return server.definition(message)
	case "textDocument/implementation":
		return server.implementation(message)
	case "textDocument/codeAction":
		return server.codeAction(message)
	case "textDocument/documentLink":
		return server.documentLinks(message)
	case "textDocument/semanticTokens/full":
		return server.semanticTokens(message)
	default:
		return server.writer.failure(
			message.ID,
			methodNotFoundCode,
			fmt.Sprintf("unsupported LSP method %q", message.Method),
		)
	}
}

type initializeParams struct {
	RootURI               string            `json:"rootUri"`
	RootPath              string            `json:"rootPath"`
	WorkspaceFolders      []workspaceFolder `json:"workspaceFolders"`
	InitializationOptions *analysisSettings `json:"initializationOptions"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type analysisSettings struct {
	Target   string                `json:"target"`
	Patterns []string              `json:"patterns"`
	Profile  compilerstyle.Profile `json:"profile"`
}

func (server *Server) initialize(message rpcMessage) error {
	if !message.request() {
		return errors.New("LSP initialize must be a request")
	}
	var params initializeParams
	if err := decodeParams(message.Params, &params); err != nil {
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			err.Error(),
		)
	}
	server.mu.Lock()
	if server.initialized {
		server.mu.Unlock()
		return server.writer.failure(
			message.ID,
			invalidRequestCode,
			"Spice language server is already initialized",
		)
	}
	server.mu.Unlock()

	settings, err := normalizeAnalysisSettings(params.InitializationOptions)
	if err != nil {
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			err.Error(),
		)
	}
	server.mu.Lock()
	server.target = settings.Target
	server.patterns = settings.Patterns
	server.profile = settings.Profile
	server.mu.Unlock()

	folders := slices.Clone(params.WorkspaceFolders)
	if len(folders) == 0 && params.RootURI != "" {
		folders = []workspaceFolder{{URI: params.RootURI}}
	}
	if len(folders) == 0 && params.RootPath != "" {
		uri, err := fileURI(params.RootPath)
		if err != nil {
			return server.writer.failure(
				message.ID,
				invalidParamsCode,
				err.Error(),
			)
		}
		folders = []workspaceFolder{{URI: uri}}
	}
	for _, folder := range folders {
		if err := server.addWorkspace(folder.URI); err != nil {
			return server.writer.failure(
				message.ID,
				internalErrorCode,
				err.Error(),
			)
		}
	}
	server.mu.Lock()
	server.initialized = true
	server.mu.Unlock()
	return server.writer.response(message.ID, initializeResult())
}

func initializeResult() map[string]any {
	return map[string]any{
		"capabilities": map[string]any{
			"positionEncoding": "utf-16",
			"textDocumentSync": map[string]any{
				"openClose": true,
				"change":    1,
				"save": map[string]any{
					"includeText": true,
				},
			},
			"completionProvider": map[string]any{
				"triggerCharacters": []string{"@", ".", "(", ",", "\""},
			},
			"hoverProvider": true,
			"signatureHelpProvider": map[string]any{
				"triggerCharacters":   []string{"(", ","},
				"retriggerCharacters": []string{","},
			},
			"definitionProvider":     true,
			"implementationProvider": true,
			"codeActionProvider": map[string]any{
				"codeActionKinds": []string{"quickfix"},
				"resolveProvider": false,
			},
			"executeCommandProvider": map[string]any{
				"commands": []string{
					annotationToolPreviewCommand,
					annotationToolApplyCommand,
				},
			},
			"documentLinkProvider": map[string]any{
				"resolveProvider": false,
			},
			"semanticTokensProvider": map[string]any{
				"legend": map[string]any{
					"tokenTypes": []string{
						"decorator",
						"parameter",
						"string",
						"number",
						"keyword",
						"operator",
					},
					"tokenModifiers": []string{},
				},
				"full": true,
			},
			"workspace": map[string]any{
				"workspaceFolders": map[string]any{
					"supported":           true,
					"changeNotifications": true,
				},
			},
		},
		"serverInfo": map[string]any{
			"name":    "spice",
			"version": "0.1.0-dev",
		},
	}
}

func (server *Server) handleShutdown(message rpcMessage) error {
	if !message.request() {
		return nil
	}
	server.mu.Lock()
	server.shutdown = true
	server.cancelAnalysesLocked()
	server.mu.Unlock()
	return server.writer.response(message.ID, nil)
}

func (server *Server) stop() error {
	server.mu.Lock()
	server.closing = true
	server.cancelAnalysesLocked()
	if server.cancel != nil {
		server.cancel()
	}
	keys := make([]string, 0, len(server.workspaces))
	for key := range server.workspaces {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	services := make([]*compilerservice.Service, 0, len(keys))
	for _, key := range keys {
		workspace := server.workspaces[key]
		if workspace != nil && workspace.service != nil {
			services = append(services, workspace.service)
		}
	}
	server.mu.Unlock()
	server.analysisWait.Wait()
	server.requestWait.Wait()
	closeCtx, cancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer cancel()
	var result error
	for _, service := range services {
		result = errors.Join(result, service.Close(closeCtx))
	}
	server.mu.Lock()
	asyncErr := server.asyncErr
	server.mu.Unlock()
	return errors.Join(result, asyncErr)
}

func (server *Server) cancelAnalysesLocked() {
	for _, workspace := range server.workspaces {
		if workspace.timer != nil {
			workspace.timer.Stop()
			workspace.timer = nil
		}
		if workspace.cancel != nil {
			workspace.cancel()
			workspace.cancel = nil
		}
	}
	for key, cancel := range server.requests {
		cancel()
		delete(server.requests, key)
	}
}

func (server *Server) shutdownState() bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.shutdown
}

func (server *Server) cancelRequest(raw json.RawMessage) {
	var params struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	server.mu.Lock()
	cancel := server.requests[string(params.ID)]
	server.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func decodeParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("invalid LSP parameters: %w", err)
	}
	return nil
}

func (server *Server) addWorkspace(uri string) error {
	root, err := filePath(uri)
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("inspect LSP workspace %q: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("LSP workspace %q is not a directory", root)
	}
	service, err := server.config.newService(root)
	if err != nil {
		return fmt.Errorf("configure LSP workspace %q: %w", root, err)
	}
	key := pathKey(root)
	server.mu.Lock()
	defer server.mu.Unlock()
	if _, found := server.workspaces[key]; found {
		return nil
	}
	server.workspaces[key] = &workspace{
		root:      root,
		uri:       uri,
		service:   service,
		target:    server.target,
		patterns:  slices.Clone(server.patterns),
		profile:   server.profile,
		published: make(map[string]struct{}),
		documents: make(map[string]struct{}),
	}
	return nil
}

func normalizeAnalysisSettings(
	settings *analysisSettings,
) (analysisSettings, error) {
	if settings == nil {
		return analysisSettings{}, nil
	}
	result := analysisSettings{
		Target:   strings.TrimSpace(settings.Target),
		Patterns: slices.Clone(settings.Patterns),
		Profile:  settings.Profile,
	}
	if result.Target != settings.Target {
		return analysisSettings{}, errors.New(
			"LSP application target must be trimmed",
		)
	}
	if err := compilerstyle.ValidateProfile(result.Profile); err != nil {
		return analysisSettings{}, err
	}
	for index, pattern := range result.Patterns {
		if strings.TrimSpace(pattern) == "" ||
			strings.TrimSpace(pattern) != pattern {
			return analysisSettings{}, fmt.Errorf(
				"LSP package pattern %d must be non-empty and trimmed",
				index,
			)
		}
	}
	return result, nil
}

func (server *Server) removeWorkspace(uri string) {
	root, err := filePath(uri)
	if err != nil {
		return
	}
	key := pathKey(root)
	server.mu.Lock()
	workspace, found := server.workspaces[key]
	if !found {
		server.mu.Unlock()
		return
	}
	if workspace.timer != nil {
		workspace.timer.Stop()
	}
	if workspace.cancel != nil {
		workspace.cancel()
	}
	delete(server.workspaces, key)
	for documentURI := range workspace.documents {
		if document := server.documents[documentURI]; document != nil {
			document.root = ""
		}
	}
	published := mapKeys(workspace.published)
	server.requestWait.Add(1)
	server.mu.Unlock()
	go func() {
		defer server.requestWait.Done()
		workspace.analysis.Wait()
		closeCtx, cancel := context.WithTimeout(
			context.Background(),
			10*time.Second,
		)
		defer cancel()
		if closeErr := workspace.service.Close(closeCtx); closeErr != nil {
			server.mu.Lock()
			server.asyncErr = errors.Join(server.asyncErr, closeErr)
			runCancel := server.cancel
			server.mu.Unlock()
			if runCancel != nil {
				runCancel()
			}
		}
	}()
	for _, documentURI := range published {
		if err := server.publishDiagnostics(
			documentURI,
			nil,
			nil,
		); err != nil {
			server.handleWriteError(err)
			return
		}
	}
}

func (server *Server) handleWriteError(err error) {
	if err == nil {
		return
	}
	server.mu.Lock()
	cancel := server.cancel
	server.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (server *Server) workspaceForPathLocked(
	path string,
) (string, *workspace) {
	var selectedKey string
	var selected *workspace
	for key, candidate := range server.workspaces {
		relative, err := filepath.Rel(candidate.root, path)
		if err != nil ||
			(relative != "." && !filepath.IsLocal(relative)) {
			continue
		}
		if selected == nil || len(candidate.root) > len(selected.root) {
			selectedKey = key
			selected = candidate
		}
	}
	return selectedKey, selected
}

func (server *Server) ensureWorkspace(path string) (string, error) {
	server.mu.Lock()
	key, found := server.workspaceForPathLocked(path)
	server.mu.Unlock()
	if found != nil {
		return key, nil
	}
	root, err := discoverModuleRoot(path)
	if err != nil {
		return "", err
	}
	uri, err := fileURI(root)
	if err != nil {
		return "", err
	}
	if err := server.addWorkspace(uri); err != nil {
		return "", err
	}
	return pathKey(root), nil
}

func discoverModuleRoot(path string) (string, error) {
	directory := filepath.Dir(path)
	for {
		info, err := os.Stat(filepath.Join(directory, "go.mod"))
		if err == nil && info.Mode().IsRegular() {
			return directory, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf(
				"inspect Go module root from %q: %w",
				path,
				err,
			)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf(
				"find Go module root for LSP document %q: go.mod not found",
				path,
			)
		}
		directory = parent
	}
}

func filePath(identity string) (string, error) {
	parsed, err := url.Parse(identity)
	if err != nil {
		return "", fmt.Errorf("parse file URI %q: %w", identity, err)
	}
	if parsed.Scheme == "" {
		return filepath.Abs(identity)
	}
	if parsed.Scheme != "file" {
		return "", fmt.Errorf("LSP URI %q must use file scheme", identity)
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return "", fmt.Errorf("LSP URI %q must not name a remote host", identity)
	}
	path := parsed.Path
	if runtime.GOOS == "windows" &&
		len(path) >= 3 &&
		path[0] == '/' &&
		path[2] == ':' {
		path = path[1:]
	}
	return filepath.Abs(filepath.FromSlash(path))
}

func fileURI(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve file URI path %q: %w", path, err)
	}
	slashPath := filepath.ToSlash(absolute)
	if runtime.GOOS == "windows" && !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String(), nil
}

func pathKey(path string) string {
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func mapKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}
