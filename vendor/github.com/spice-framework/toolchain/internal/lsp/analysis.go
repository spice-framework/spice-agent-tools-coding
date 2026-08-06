package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/spice-framework/toolchain/compiler/diagnostic"
	compilerservice "github.com/spice-framework/toolchain/compiler/service"
)

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type versionedTextDocument struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type contentChange struct {
	Range       *protocolRange `json:"range,omitempty"`
	RangeLength *int           `json:"rangeLength,omitempty"`
	Text        string         `json:"text"`
}

type didChangeParams struct {
	TextDocument   versionedTextDocument `json:"textDocument"`
	ContentChanges []contentChange       `json:"contentChanges"`
}

type didSaveParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Text         *string                `json:"text,omitempty"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type workspaceFoldersChangeParams struct {
	Event struct {
		Added   []workspaceFolder `json:"added"`
		Removed []workspaceFolder `json:"removed"`
	} `json:"event"`
}

type didChangeConfigurationParams struct {
	Settings struct {
		Spice *analysisSettings `json:"spice"`
	} `json:"settings"`
}

func (server *Server) didOpen(raw json.RawMessage) {
	var params didOpenParams
	if decodeParams(raw, &params) != nil ||
		params.TextDocument.URI == "" ||
		len(params.TextDocument.Text) > server.config.maxDocumentBytes {
		return
	}
	path, err := filePath(params.TextDocument.URI)
	if err != nil {
		return
	}
	root, err := server.ensureWorkspace(path)
	if err != nil {
		server.showError(err)
		return
	}
	server.mu.Lock()
	if previous := server.documents[params.TextDocument.URI]; previous != nil {
		if workspace := server.workspaces[previous.root]; workspace != nil {
			delete(workspace.documents, previous.uri)
		}
	}
	server.documents[params.TextDocument.URI] = &document{
		uri:      params.TextDocument.URI,
		path:     path,
		language: params.TextDocument.LanguageID,
		version:  params.TextDocument.Version,
		content:  []byte(params.TextDocument.Text),
		root:     root,
	}
	if workspace := server.workspaces[root]; workspace != nil {
		workspace.documents[params.TextDocument.URI] = struct{}{}
		server.scheduleWorkspaceLocked(root, workspace)
	}
	server.mu.Unlock()
}

func (server *Server) didChange(raw json.RawMessage) {
	var params didChangeParams
	if decodeParams(raw, &params) != nil ||
		len(params.ContentChanges) == 0 {
		return
	}
	change := params.ContentChanges[len(params.ContentChanges)-1]
	if change.Range != nil ||
		len(change.Text) > server.config.maxDocumentBytes {
		return
	}
	server.mu.Lock()
	document := server.documents[params.TextDocument.URI]
	if document == nil || params.TextDocument.Version <= document.version {
		server.mu.Unlock()
		return
	}
	document.version = params.TextDocument.Version
	document.content = []byte(change.Text)
	if workspace := server.workspaces[document.root]; workspace != nil {
		server.scheduleWorkspaceLocked(document.root, workspace)
	}
	server.mu.Unlock()
}

func (server *Server) didSave(raw json.RawMessage) {
	var params didSaveParams
	if decodeParams(raw, &params) != nil {
		return
	}
	server.mu.Lock()
	document := server.documents[params.TextDocument.URI]
	if document == nil {
		server.mu.Unlock()
		return
	}
	if params.Text != nil {
		if len(*params.Text) > server.config.maxDocumentBytes {
			server.mu.Unlock()
			return
		}
		document.content = []byte(*params.Text)
	}
	if workspace := server.workspaces[document.root]; workspace != nil {
		server.scheduleWorkspaceLocked(document.root, workspace)
	}
	server.mu.Unlock()
}

func (server *Server) didClose(raw json.RawMessage) error {
	var params didCloseParams
	if err := decodeParams(raw, &params); err != nil {
		return err
	}
	server.mu.Lock()
	document := server.documents[params.TextDocument.URI]
	if document != nil {
		delete(server.documents, params.TextDocument.URI)
		if workspace := server.workspaces[document.root]; workspace != nil {
			delete(workspace.documents, params.TextDocument.URI)
			delete(workspace.published, params.TextDocument.URI)
			server.scheduleWorkspaceLocked(document.root, workspace)
		}
	}
	server.mu.Unlock()
	if document != nil {
		return server.publishDiagnostics(params.TextDocument.URI, nil, nil)
	}
	return nil
}

func (server *Server) didChangeWorkspaceFolders(raw json.RawMessage) {
	var params workspaceFoldersChangeParams
	if decodeParams(raw, &params) != nil {
		return
	}
	for _, folder := range params.Event.Removed {
		server.removeWorkspace(folder.URI)
	}
	for _, folder := range params.Event.Added {
		if err := server.addWorkspace(folder.URI); err != nil {
			server.showError(err)
		}
	}
}

func (server *Server) refreshAll(raw json.RawMessage) {
	var params didChangeConfigurationParams
	if decodeParams(raw, &params) != nil {
		return
	}
	settings, err := normalizeAnalysisSettings(params.Settings.Spice)
	if err != nil {
		server.showError(err)
		return
	}
	server.mu.Lock()
	if params.Settings.Spice != nil {
		server.target = settings.Target
		server.patterns = slices.Clone(settings.Patterns)
	}
	for key, workspace := range server.workspaces {
		if params.Settings.Spice != nil {
			workspace.target = settings.Target
			workspace.patterns = slices.Clone(settings.Patterns)
		}
		server.scheduleWorkspaceLocked(key, workspace)
	}
	server.mu.Unlock()
}

func (server *Server) scheduleWorkspaceLocked(
	key string,
	workspace *workspace,
) {
	if server.closing || server.shutdown {
		return
	}
	workspace.sequence++
	sequence := workspace.sequence
	if workspace.timer != nil {
		workspace.timer.Stop()
	}
	if workspace.cancel != nil {
		workspace.cancel()
		workspace.cancel = nil
	}
	workspace.timer = time.AfterFunc(
		server.config.analysisDelay,
		func() {
			server.beginAnalysis(key, sequence)
		},
	)
}

type analysisSnapshot struct {
	root     string
	target   string
	patterns []string
	sequence uint64
	overlay  map[string]compilerservice.Document
	versions map[string]int
	service  *compilerservice.Service
}

func (server *Server) beginAnalysis(key string, sequence uint64) {
	server.mu.Lock()
	workspace := server.workspaces[key]
	if workspace == nil ||
		workspace.sequence != sequence ||
		server.closing ||
		server.shutdown {
		server.mu.Unlock()
		return
	}
	workspace.timer = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := server.done
	workspace.cancel = cancel
	snapshot := analysisSnapshot{
		root:     workspace.root,
		target:   workspace.target,
		patterns: slices.Clone(workspace.patterns),
		sequence: sequence,
		overlay: make(
			map[string]compilerservice.Document,
			len(workspace.documents),
		),
		versions: make(map[string]int, len(workspace.documents)),
		service:  workspace.service,
	}
	for uri := range workspace.documents {
		document := server.documents[uri]
		if document == nil {
			continue
		}
		snapshot.overlay[uri] = compilerservice.Document{
			Version: document.version,
			Content: slices.Clone(document.content),
		}
		snapshot.versions[uri] = document.version
	}
	server.analysisWait.Add(1)
	workspace.analysis.Add(1)
	server.mu.Unlock()

	defer server.analysisWait.Done()
	defer workspace.analysis.Done()
	go cancelWhenDone(ctx, cancel, done)
	result, err := snapshot.service.Analyze(
		ctx,
		compilerservice.Request{
			WorkspaceRoot: snapshot.root,
			Target:        snapshot.target,
			Patterns:      snapshot.patterns,
			Overlay:       snapshot.overlay,
			Sequence:      snapshot.sequence,
		},
	)
	cancel()
	if err != nil {
		if errors.Is(err, context.Canceled) ||
			errors.Is(err, compilerservice.ErrStaleAnalysis) {
			return
		}
		server.showError(err)
		return
	}
	server.completeAnalysis(key, snapshot, result)
}

func (server *Server) completeAnalysis(
	key string,
	snapshot analysisSnapshot,
	result compilerservice.Result,
) {
	server.mu.Lock()
	workspace := server.workspaces[key]
	if workspace == nil ||
		workspace.sequence != snapshot.sequence ||
		server.closing ||
		!server.versionsCurrentLocked(snapshot.versions) {
		server.mu.Unlock()
		return
	}
	workspace.cancel = nil
	workspace.latest = result
	workspace.hasLatest = true
	if result.GenerationReady() {
		workspace.lastGood = result
		workspace.hasGood = true
	}
	publications, globalMessages := server.publicationsLocked(
		workspace,
		result,
	)
	server.mu.Unlock()

	for _, publication := range publications {
		if !server.sequenceCurrent(key, snapshot.sequence) {
			return
		}
		if err := server.publishDiagnostics(
			publication.uri,
			publication.version,
			publication.diagnostics,
		); err != nil {
			return
		}
	}
	for _, message := range globalMessages {
		if !server.sequenceCurrent(key, snapshot.sequence) {
			return
		}
		if err := server.writer.notification("window/showMessage", map[string]any{
			"type":    1,
			"message": message,
		}); err != nil {
			server.handleWriteError(err)
			return
		}
	}
}

func cancelWhenDone(
	ctx context.Context,
	cancel context.CancelFunc,
	done <-chan struct{},
) {
	select {
	case <-done:
		cancel()
	case <-ctx.Done():
	}
}

func (server *Server) versionsCurrentLocked(
	versions map[string]int,
) bool {
	for uri, version := range versions {
		document := server.documents[uri]
		if document == nil || document.version != version {
			return false
		}
	}
	return true
}

func (server *Server) sequenceCurrent(key string, sequence uint64) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	workspace := server.workspaces[key]
	return workspace != nil &&
		workspace.sequence == sequence &&
		!server.closing
}

type diagnosticPublication struct {
	uri         string
	version     *int
	diagnostics []protocolDiagnostic
}

func (server *Server) publicationsLocked(
	workspace *workspace,
	result compilerservice.Result,
) ([]diagnosticPublication, []string) {
	grouped := make(map[string][]protocolDiagnostic)
	var global []string
	for _, item := range result.Diagnostics().Items() {
		if item.Location.URI == "" || item.Location.Path == "" {
			global = append(global, item.Message)
			continue
		}
		content := server.locationContentLocked(item.Location)
		converted := protocolDiagnosticFromCompiler(item, content)
		grouped[item.Location.URI] = append(
			grouped[item.Location.URI],
			converted,
		)
	}
	uris := mapKeys(workspace.published)
	for uri := range workspace.documents {
		if !slices.Contains(uris, uri) {
			uris = append(uris, uri)
		}
	}
	for uri := range grouped {
		if !slices.Contains(uris, uri) {
			uris = append(uris, uri)
		}
	}
	sort.Strings(uris)
	publications := make([]diagnosticPublication, 0, len(uris))
	workspace.published = make(map[string]struct{}, len(grouped))
	for _, uri := range uris {
		items := grouped[uri]
		if len(items) != 0 {
			workspace.published[uri] = struct{}{}
		}
		var version *int
		if document := server.documents[uri]; document != nil {
			current := document.version
			version = &current
		}
		publications = append(publications, diagnosticPublication{
			uri:         uri,
			version:     version,
			diagnostics: items,
		})
	}
	return publications, global
}

func (server *Server) locationContentLocked(
	location diagnostic.Location,
) []byte {
	if document := server.documents[location.URI]; document != nil {
		return slices.Clone(document.content)
	}
	for _, document := range server.documents {
		if pathKey(document.path) == pathKey(location.Path) {
			return slices.Clone(document.content)
		}
	}
	info, err := os.Stat(location.Path)
	if err != nil ||
		!info.Mode().IsRegular() ||
		info.Size() > int64(server.config.maxDocumentBytes) {
		return nil
	}
	content, err := os.ReadFile(filepath.Clean(location.Path))
	if err != nil || len(content) > server.config.maxDocumentBytes {
		return nil
	}
	return content
}

func (server *Server) publishDiagnostics(
	uri string,
	version *int,
	diagnostics []protocolDiagnostic,
) error {
	if diagnostics == nil {
		diagnostics = []protocolDiagnostic{}
	}
	params := map[string]any{
		"uri":         uri,
		"diagnostics": diagnostics,
	}
	if version != nil {
		params["version"] = *version
	}
	return server.writer.notification(
		"textDocument/publishDiagnostics",
		params,
	)
}

func (server *Server) showError(err error) {
	if err == nil || server.writer == nil {
		return
	}
	if writeErr := server.writer.notification("window/showMessage", map[string]any{
		"type":    1,
		"message": "Spice analysis failed: " + err.Error(),
	}); writeErr != nil {
		server.handleWriteError(writeErr)
	}
}

type protocolPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type protocolRange struct {
	Start protocolPosition `json:"start"`
	End   protocolPosition `json:"end"`
}

type protocolLocation struct {
	URI   string        `json:"uri"`
	Range protocolRange `json:"range"`
}

type protocolRelatedInformation struct {
	Location protocolLocation `json:"location"`
	Message  string           `json:"message"`
}

type protocolDiagnostic struct {
	Range    protocolRange                `json:"range"`
	Severity int                          `json:"severity"`
	Code     string                       `json:"code"`
	Source   string                       `json:"source"`
	Message  string                       `json:"message"`
	Related  []protocolRelatedInformation `json:"relatedInformation,omitempty"`
}

func protocolDiagnosticFromCompiler(
	item diagnostic.Diagnostic,
	content []byte,
) protocolDiagnostic {
	result := protocolDiagnostic{
		Range:    protocolRangeFromCompiler(item.Location.Range, content),
		Severity: protocolSeverity(item.Severity),
		Code:     item.Code,
		Source:   "spice",
		Message:  item.Message,
	}
	for _, related := range item.Related {
		if related.Location.URI == "" {
			continue
		}
		result.Related = append(
			result.Related,
			protocolRelatedInformation{
				Location: protocolLocation{
					URI: related.Location.URI,
					Range: protocolRangeFromCompiler(
						related.Location.Range,
						nil,
					),
				},
				Message: related.Message,
			},
		)
	}
	return result
}

func protocolSeverity(severity diagnostic.Severity) int {
	switch severity {
	case diagnostic.SeverityError:
		return 1
	case diagnostic.SeverityWarning:
		return 2
	case diagnostic.SeverityInformation:
		return 3
	case diagnostic.SeverityHint:
		return 4
	default:
		return 1
	}
}

func protocolRangeFromCompiler(
	item diagnostic.Range,
	content []byte,
) protocolRange {
	return protocolRange{
		Start: protocolPositionFromCompiler(item.Start, content),
		End:   protocolPositionFromCompiler(item.End, content),
	}
}

func protocolPositionFromCompiler(
	position diagnostic.Position,
	content []byte,
) protocolPosition {
	line := max(position.Line-1, 0)
	character := max(position.Column-1, 0)
	if len(content) == 0 {
		return protocolPosition{Line: line, Character: character}
	}
	start, end, found := contentLine(content, line+1)
	if !found {
		return protocolPosition{Line: line, Character: character}
	}
	byteColumn := min(max(position.Column-1, 0), end-start)
	character = utf16Length(content[start : start+byteColumn])
	return protocolPosition{Line: line, Character: character}
}

func utf16Length(content []byte) int {
	units := 0
	for len(content) != 0 {
		character, size := utf8.DecodeRune(content)
		if size == 0 {
			break
		}
		if character > 0xFFFF {
			units += 2
		} else {
			units++
		}
		content = content[size:]
	}
	return units
}
