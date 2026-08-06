// Package web provides the small HTTP runtime used by generated Spice
// controller adapters.
package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spice-framework/spice/conversion"
)

const (
	// DefaultMaxBodyBytes is the generated-adapter request body limit unless an
	// application selects a different positive bound.
	DefaultMaxBodyBytes int64 = 1 << 20

	problemMediaType = "application/problem+json"
	jsonMediaType    = "application/json"
)

// NoContent is an explicit controller response that generates HTTP 204.
type NoContent struct{}

// Problem is an RFC 9457 problem-details document.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// Validate checks the stable problem contract before a document is exposed.
func (p Problem) Validate() error {
	if p.Status < http.StatusBadRequest || p.Status > 599 {
		return fmt.Errorf("problem status %d must be between 400 and 599", p.Status)
	}
	if p.Type != "" {
		if _, err := url.Parse(p.Type); err != nil {
			return fmt.Errorf("problem type %q is not a URI reference: %w", p.Type, err)
		}
	}
	if p.Instance != "" {
		if _, err := url.Parse(p.Instance); err != nil {
			return fmt.Errorf("problem instance %q is not a URI reference: %w", p.Instance, err)
		}
	}
	if p.Title == "" && http.StatusText(p.Status) == "" {
		return errors.New("problem title is required when the status has no standard title")
	}
	return nil
}

// ProblemCarrier exposes a safe client-facing problem for an error.
type ProblemCarrier interface {
	Problem() Problem
}

// HTTPError associates a safe problem document with an optional internal
// cause. The cause is available to logs through errors.Unwrap but is never
// included by the default response mapper.
type HTTPError struct {
	problem Problem
	cause   error
}

// NewError creates a problem-carrying HTTP error.
func NewError(problem Problem, cause error) *HTTPError {
	return &HTTPError{problem: problem, cause: cause}
}

// Error returns stable problem context without rendering the internal cause.
func (e *HTTPError) Error() string {
	if e == nil {
		return "HTTP error"
	}
	problem := normalizeProblem(e.problem)
	if problem.Detail == "" {
		return problem.Title
	}
	return problem.Title + ": " + problem.Detail
}

// Unwrap returns the internal cause.
func (e *HTTPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Problem returns the safe client-facing problem.
func (e *HTTPError) Problem() Problem {
	if e == nil {
		return internalProblem()
	}
	return e.problem
}

// Location identifies one request binding source.
type Location string

const (
	// LocationPath identifies a route path variable.
	LocationPath Location = "path"
	// LocationQuery identifies a URL query parameter.
	LocationQuery Location = "query"
	// LocationHeader identifies an HTTP request header.
	LocationHeader Location = "header"
	// LocationBody identifies the request body.
	LocationBody Location = "body"
	// LocationForm identifies a URL-encoded form field.
	LocationForm Location = "form"
	// LocationRequest identifies whole-request typed validation.
	LocationRequest Location = "request"
)

// BindingError is a safe source-specific bad-request error. It intentionally
// records no raw request value.
type BindingError struct {
	Location Location
	Field    string
	Reason   string
	cause    error
}

// NewBindingError creates a request binding failure without retaining the raw
// client value.
func NewBindingError(location Location, field, reason string, cause error) *BindingError {
	return &BindingError{
		Location: location,
		Field:    field,
		Reason:   reason,
		cause:    cause,
	}
}

// Error renders stable request-field context.
func (e *BindingError) Error() string {
	if e == nil {
		return "bind request"
	}
	location := e.Location
	if location == "" {
		location = "request"
	}
	field := e.Field
	if field == "" {
		field = "value"
	}
	reason := e.Reason
	if reason == "" {
		reason = "is invalid"
	}
	return fmt.Sprintf("%s %q %s", location, field, reason)
}

// Unwrap returns the parser or decoder cause for server-side diagnostics.
func (e *BindingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Problem returns a standard invalid-request problem.
func (e *BindingError) Problem() Problem {
	return Problem{
		Type:   "https://spice.dev/problems/invalid-request",
		Title:  "Invalid request",
		Status: http.StatusBadRequest,
		Detail: e.Error(),
	}
}

// ErrorMapper converts an application error into a safe problem document.
type ErrorMapper func(context.Context, error) Problem

// Middleware wraps an HTTP handler. Generated applications apply middleware in
// caller order: the first item observes the request first and the response
// last.
type Middleware func(http.Handler) http.Handler

// RouteMetadata is the stable generated identity of one HTTP route.
type RouteMetadata struct {
	ID      string
	Module  string
	Method  string
	Pattern string
}

// HTTPResult describes one completed generated route request.
type HTTPResult struct {
	Status   int
	Bytes    int64
	Duration time.Duration
	Panicked bool
}

// HTTPObserver is the dependency-free adapter point for route metrics and
// traces. Observers begin in declaration order and finish in reverse order.
type HTTPObserver interface {
	BeginHTTP(context.Context, RouteMetadata) (context.Context, func(HTTPResult))
}

// HTTPObserverFunc adapts a function to HTTPObserver.
type HTTPObserverFunc func(context.Context, RouteMetadata) (context.Context, func(HTTPResult))

// BeginHTTP implements HTTPObserver.
func (observer HTTPObserverFunc) BeginHTTP(
	ctx context.Context,
	route RouteMetadata,
) (context.Context, func(HTTPResult)) {
	return observer(ctx, route)
}

// Chain applies a deterministic middleware list around handler. Nil
// middleware and nil returned handlers are rejected during construction.
func Chain(handler http.Handler, middleware ...Middleware) (http.Handler, error) {
	if handler == nil {
		return nil, errors.New("chain HTTP middleware: handler is nil")
	}
	result := handler
	for index, item := range slices.Backward(middleware) {
		if item == nil {
			return nil, fmt.Errorf("chain HTTP middleware: middleware %d is nil", index)
		}
		result = item(result)
		if result == nil {
			return nil, fmt.Errorf("chain HTTP middleware: middleware %d returned nil", index)
		}
	}
	return result, nil
}

// ObservationMiddleware creates one route-aware middleware after validating
// generated metadata and every observer.
func ObservationMiddleware(
	route RouteMetadata,
	observers ...HTTPObserver,
) (Middleware, error) {
	if route.ID == "" {
		return nil, errors.New("observe HTTP route: route ID is empty")
	}
	if route.Method == "" || route.Method != strings.ToUpper(route.Method) {
		return nil, errors.New("observe HTTP route: method must be uppercase")
	}
	if route.Pattern == "" || route.Pattern[0] != '/' {
		return nil, errors.New("observe HTTP route: pattern must be absolute")
	}
	for index, observer := range observers {
		if nilHTTPObserver(observer) {
			return nil, fmt.Errorf("observe HTTP route: observer %d is nil", index)
		}
	}
	copied := append([]HTTPObserver(nil), observers...)
	return func(next http.Handler) http.Handler {
		if len(copied) == 0 {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			observeRequest(writer, request, next, route, copied)
		})
	}, nil
}

func nilHTTPObserver(observer HTTPObserver) bool {
	if observer == nil {
		return true
	}
	value := reflect.ValueOf(observer)
	return (value.Kind() == reflect.Func || value.Kind() == reflect.Pointer) && value.IsNil()
}

type observedResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (writer *observedResponseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.ResponseWriter.WriteHeader(status)
	writer.status = status
}

func (writer *observedResponseWriter) Write(content []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	written, err := writer.ResponseWriter.Write(content)
	writer.bytes += int64(written)
	return written, err
}

func (writer *observedResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *observedResponseWriter) Flush() {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *observedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := writer.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (writer *observedResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := writer.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func (writer *observedResponseWriter) ReadFrom(reader io.Reader) (int64, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := writer.ResponseWriter.(io.ReaderFrom); ok {
		written, err := readerFrom.ReadFrom(reader)
		writer.bytes += written
		return written, err
	}
	written, err := io.Copy(struct{ io.Writer }{writer}, reader)
	return written, err
}

func observeRequest(
	responseWriter http.ResponseWriter,
	request *http.Request,
	next http.Handler,
	route RouteMetadata,
	observers []HTTPObserver,
) {
	started := time.Now()
	writer := &observedResponseWriter{ResponseWriter: responseWriter}
	ctx := request.Context()
	finishers := make([]func(HTTPResult), 0, len(observers))
	for _, observer := range observers {
		observedContext, finish := observer.BeginHTTP(ctx, route)
		if observedContext != nil {
			ctx = observedContext //nolint:fatcontext // Ordered observers intentionally compose request-scoped trace contexts.
		}
		if finish != nil {
			finishers = append(finishers, finish)
		}
	}
	defer func() {
		recovered := recover()
		status := writer.status
		if status == 0 {
			status = http.StatusOK
		}
		if recovered != nil && writer.status == 0 {
			status = http.StatusInternalServerError
		}
		result := HTTPResult{
			Status:   status,
			Bytes:    writer.bytes,
			Duration: time.Since(started),
			Panicked: recovered != nil,
		}
		for _, finish := range slices.Backward(finishers) {
			finish(result)
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	next.ServeHTTP(writer, request.WithContext(ctx))
}

// Validate invokes one generated request validator. Explicit problem errors
// retain their policy; ordinary validator errors become a safe 400 without
// exposing their text.
func Validate(ctx context.Context, validator func(context.Context) error) error {
	if ctx == nil {
		return errors.New("validate request: context is nil")
	}
	if validator == nil {
		return errors.New("validate request: validator is nil")
	}
	err := validator(ctx)
	if err == nil {
		return nil
	}
	var carrier ProblemCarrier
	if errors.As(err, &carrier) {
		return err
	}
	return NewBindingError(LocationRequest, "request", "failed validation", err)
}

// Register safely registers one generated route. ServeMux reports invalid or
// conflicting patterns by panic; Spice converts that programmer/configuration
// fault into an application-construction error so cleanup rollback still runs.
func Register(
	mux *http.ServeMux,
	pattern string,
	handler http.Handler,
	middleware ...Middleware,
) (err error) {
	if mux == nil {
		return errors.New("register HTTP route: mux is nil")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("register HTTP route %q: %v", pattern, recovered)
		}
	}()
	routeHandler, chainErr := Chain(handler, middleware...)
	if chainErr != nil {
		return fmt.Errorf("register HTTP route %q: %w", pattern, chainErr)
	}
	mux.Handle(pattern, routeHandler)
	return nil
}

// RegisterObserved applies caller middleware inside one generated observation
// middleware, then safely registers the route. Caller middleware errors retain
// their original list indexes.
func RegisterObserved(
	mux *http.ServeMux,
	pattern string,
	handler http.Handler,
	observation Middleware,
	middleware ...Middleware,
) (err error) {
	if mux == nil {
		return errors.New("register observed HTTP route: mux is nil")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("register observed HTTP route %q: %v", pattern, recovered)
		}
	}()
	routeHandler, chainErr := Chain(handler, middleware...)
	if chainErr != nil {
		return fmt.Errorf("register observed HTTP route %q: %w", pattern, chainErr)
	}
	return Register(mux, pattern, routeHandler, observation)
}

// DefaultErrorMapper preserves valid explicit problems and otherwise returns a
// generic 500 response without leaking internal error text.
func DefaultErrorMapper(_ context.Context, err error) Problem {
	var carrier ProblemCarrier
	if errors.As(err, &carrier) {
		problem := normalizeProblem(carrier.Problem())
		if problem.Validate() == nil {
			return problem
		}
	}
	return internalProblem()
}

// WriteError maps err and writes an RFC 9457 response. A nil mapper selects the
// secure default.
func WriteError(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
	mapper ErrorMapper,
) error {
	if mapper == nil {
		mapper = DefaultErrorMapper
	}
	ctx := context.Background()
	if request != nil {
		ctx = request.Context()
	}
	return WriteProblem(writer, mapper(ctx, err))
}

// WriteProblem writes one validated application/problem+json response. Invalid
// problem metadata is replaced with the secure internal-server problem.
func WriteProblem(writer http.ResponseWriter, problem Problem) error {
	if writer == nil {
		return errors.New("write problem: response writer is nil")
	}
	problem = normalizeProblem(problem)
	if err := problem.Validate(); err != nil {
		problem = internalProblem()
	}
	writer.Header().Set("Content-Type", problemMediaType)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(problem.Status)
	if err := json.NewEncoder(writer).Encode(problem); err != nil {
		return fmt.Errorf("write problem response: %w", err)
	}
	return nil
}

// WriteJSON writes one application/json response.
func WriteJSON(writer http.ResponseWriter, status int, value any) error {
	if writer == nil {
		return errors.New("write JSON: response writer is nil")
	}
	if status < http.StatusOK || status > 599 {
		return fmt.Errorf("write JSON: status %d must be between 200 and 599", status)
	}
	writer.Header().Set("Content-Type", jsonMediaType)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		return fmt.Errorf("write JSON response: %w", err)
	}
	return nil
}

// WriteNoContent writes an explicit HTTP 204 response.
func WriteNoContent(writer http.ResponseWriter) error {
	if writer == nil {
		return errors.New("write no-content response: response writer is nil")
	}
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Del("Content-Type")
	writer.WriteHeader(http.StatusNoContent)
	return nil
}

// AcceptsJSON reports whether an Accept header permits a JSON representation.
// An empty header accepts JSON.
func AcceptsJSON(header string) bool {
	if strings.TrimSpace(header) == "" {
		return true
	}
	for part := range strings.SplitSeq(header, ",") {
		mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || qualityRejected(parameters["q"]) {
			continue
		}
		if mediaType == "*/*" || mediaType == "application/*" ||
			mediaType == jsonMediaType || strings.HasSuffix(mediaType, "+json") {
			return true
		}
	}
	return false
}

func qualityRejected(value string) bool {
	if value == "" {
		return false
	}
	quality, err := strconv.ParseFloat(value, 64)
	return err != nil || quality <= 0 || quality > 1
}

// DecodeJSON strictly decodes exactly one bounded application/json request
// body. Unknown fields and trailing values fail closed.
func DecodeJSON(request *http.Request, destination any, maxBytes int64) error {
	if request == nil {
		return errors.New("decode JSON request: request is nil")
	}
	if destination == nil {
		return errors.New("decode JSON request: destination is nil")
	}
	if maxBytes == 0 {
		maxBytes = DefaultMaxBodyBytes
	}
	if maxBytes < 0 {
		return errors.New("decode JSON request: max bytes must not be negative")
	}
	if err := requireJSONContentType(request.Header.Get("Content-Type")); err != nil {
		return err
	}
	if request.ContentLength > maxBytes {
		return NewBindingError(LocationBody, "body", "exceeds the configured byte limit", nil)
	}
	if request.Body == nil {
		return NewBindingError(LocationBody, "body", "is required", nil)
	}
	content, err := io.ReadAll(io.LimitReader(request.Body, maxBytes+1))
	if err != nil {
		return NewBindingError(LocationBody, "body", "could not be read", err)
	}
	if int64(len(content)) > maxBytes {
		return NewBindingError(LocationBody, "body", "exceeds the configured byte limit", nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return NewBindingError(LocationBody, "body", "must contain one valid JSON value matching the request schema", err)
	}
	if err := requireJSONEnd(decoder); err != nil {
		return err
	}
	return nil
}

func requireJSONContentType(header string) error {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil || (mediaType != jsonMediaType && !strings.HasSuffix(mediaType, "+json")) {
		return NewBindingError(LocationHeader, "Content-Type", "must be application/json", err)
	}
	return nil
}

func requireJSONEnd(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return NewBindingError(LocationBody, "body", "contains invalid trailing content", err)
	}
	return NewBindingError(LocationBody, "body", "must contain exactly one JSON value", nil)
}

// Parameter returns zero or one request parameter value. Required and repeated
// values fail with safe location-aware errors.
func Parameter(location Location, name string, values []string, required bool) (string, bool, error) {
	switch len(values) {
	case 0:
		if required {
			return "", false, NewBindingError(location, name, "is required", nil)
		}
		return "", false, nil
	case 1:
		return values[0], true, nil
	default:
		return "", false, NewBindingError(location, name, "must be provided at most once", nil)
	}
}

// Boolean parses a Boolean parameter without retaining or reporting raw input.
func Boolean(location Location, name, raw string) (bool, error) {
	value, err := conversion.ParseBoolean(raw)
	if err != nil {
		return false, NewBindingError(location, name, "must be a boolean", err)
	}
	return value, nil
}

// Integer parses a base-10 signed parameter with the requested Go bit width.
func Integer(location Location, name, raw string, bitSize int) (int64, error) {
	value, err := conversion.ParseSignedInteger(raw, bitSize)
	if err != nil {
		return 0, NewBindingError(location, name, "must be a signed base-10 integer in range", err)
	}
	return value, nil
}

// Duration parses a Go duration parameter without retaining raw input.
func Duration(location Location, name, raw string) (time.Duration, error) {
	value, err := conversion.ParseDuration(raw)
	if err != nil {
		return 0, NewBindingError(location, name, "must be a Go duration", err)
	}
	return value, nil
}

func normalizeProblem(problem Problem) Problem {
	if problem.Type == "" {
		problem.Type = "about:blank"
	}
	if problem.Title == "" {
		problem.Title = http.StatusText(problem.Status)
	}
	return problem
}

func internalProblem() Problem {
	return Problem{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
	}
}
