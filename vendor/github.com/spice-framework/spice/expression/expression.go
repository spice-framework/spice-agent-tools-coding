// Package expression provides a small, typed, reflection-free expression
// language for compiler-validated framework policies.
//
// Expressions can use explicitly declared Boolean and string variables and
// functions. They support !, &&, ||, ==, !=, literals, calls, and parentheses.
// There is no property traversal, bean lookup, method invocation, assignment,
// allocation, I/O, or access to ambient process state.
package expression

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maximumSourceBytes = 4096
	maximumTokens      = 512
	maximumDepth       = 64
)

// Kind identifies one expression value type.
type Kind uint8

const (
	invalidKind Kind = iota
	// Boolean identifies a Boolean expression value.
	Boolean
	// String identifies a string expression value.
	String
)

// Variable declares one positional input available to an expression.
type Variable struct {
	Name string
	Kind Kind
}

// FunctionSpec declares one positional function available to an expression.
type FunctionSpec struct {
	Name       string
	Parameters []Kind
	Result     Kind
}

// Schema is the complete immutable symbol surface accepted by Compile.
// Declaration order becomes the positional runtime binding order.
type Schema struct {
	Variables []Variable
	Functions []FunctionSpec
}

// Value is one immutable typed expression value.
type Value struct {
	kind    Kind
	boolean bool
	text    string
}

// Bool constructs a Boolean value.
func Bool(value bool) Value {
	return Value{kind: Boolean, boolean: value}
}

// Text constructs a string value.
func Text(value string) Value {
	return Value{kind: String, text: value}
}

// Kind reports the value's expression type.
func (value Value) Kind() Kind {
	return value.kind
}

// BooleanValue returns a Boolean value and whether its type matches.
func (value Value) BooleanValue() (bool, bool) {
	return value.boolean, value.kind == Boolean
}

// StringValue returns a string value and whether its type matches.
func (value Value) StringValue() (string, bool) {
	return value.text, value.kind == String
}

// Function evaluates one declared function. Arguments and results are checked
// against the compiled schema before values enter or leave the function.
type Function func(context.Context, []Value) (Value, error)

// Inputs binds variables and functions in the declaration order of the schema
// passed to Compile. Slices are read only for the duration of Evaluate.
type Inputs struct {
	Variables []Value
	Functions []Function
}

// CompileError is a source-positioned expression failure. Offset is a
// zero-based UTF-8 byte offset into the expression literal.
type CompileError struct {
	Offset  int
	Message string
}

// Error renders a bounded message without including the expression source.
func (err *CompileError) Error() string {
	if err == nil {
		return "compile expression"
	}
	return fmt.Sprintf("compile expression at byte %d: %s", err.Offset, err.Message)
}

// Program is an immutable compiled Boolean expression.
type Program struct {
	source    string
	root      *node
	variables []Variable
	functions []FunctionSpec
}

// Compile parses and type-checks one Boolean expression against an explicit
// schema. The schema and source are defensively copied into the returned
// immutable program.
func Compile(source string, schema Schema) (Program, error) {
	if source == "" {
		return Program{}, compileError(0, "source is required")
	}
	if source != strings.TrimSpace(source) {
		return Program{}, compileError(0, "source must not have surrounding whitespace")
	}
	if len(source) > maximumSourceBytes {
		return Program{}, compileError(maximumSourceBytes, "source exceeds 4096 bytes")
	}
	validated, variables, functions, err := validateSchema(schema)
	if err != nil {
		return Program{}, err
	}
	tokens, err := lex(source)
	if err != nil {
		return Program{}, err
	}
	parser := expressionParser{
		tokens:    tokens,
		variables: variables,
		functions: functions,
	}
	root, err := parser.parse()
	if err != nil {
		return Program{}, err
	}
	if root.kind != Boolean {
		return Program{}, compileError(root.offset, "result must be Boolean")
	}
	return Program{
		source:    source,
		root:      root,
		variables: validated.Variables,
		functions: validated.Functions,
	}, nil
}

// Source returns the canonical caller-supplied expression.
func (program Program) Source() string {
	return program.source
}

// Evaluate executes a compiled expression against exact positional bindings.
// A nil context, wrong binding count/type, nil function, cancellation, or
// function failure fails closed.
func (program Program) Evaluate(ctx context.Context, inputs Inputs) (bool, error) {
	if ctx == nil {
		return false, errors.New("evaluate expression: context is nil")
	}
	if program.root == nil {
		return false, errors.New("evaluate expression: program is invalid")
	}
	if err := validateInputs(program, inputs); err != nil {
		return false, err
	}
	value, err := evaluateNode(ctx, program, inputs, program.root)
	if err != nil {
		return false, err
	}
	result, ok := value.BooleanValue()
	if !ok {
		return false, errors.New("evaluate expression: result is not Boolean")
	}
	return result, nil
}

type nodeOperation uint8

const (
	operationLiteral nodeOperation = iota
	operationVariable
	operationCall
	operationNot
	operationAnd
	operationOr
	operationEqual
	operationNotEqual
)

type node struct {
	operation nodeOperation
	kind      Kind
	offset    int
	index     int
	literal   Value
	left      *node
	right     *node
	arguments []*node
}

type symbol struct {
	index int
	kind  Kind
}

type functionSymbol struct {
	index int
	spec  FunctionSpec
}

func validateSchema(
	schema Schema,
) (Schema, map[string]symbol, map[string]functionSymbol, error) {
	validated := Schema{
		Variables: append([]Variable(nil), schema.Variables...),
		Functions: make([]FunctionSpec, len(schema.Functions)),
	}
	variables := make(map[string]symbol, len(schema.Variables))
	functions := make(map[string]functionSymbol, len(schema.Functions))
	seen := make(map[string]string, len(schema.Variables)+len(schema.Functions))
	for index, variable := range validated.Variables {
		if err := validateSymbol("variable", variable.Name, variable.Kind); err != nil {
			return Schema{}, nil, nil, err
		}
		if previous := seen[variable.Name]; previous != "" {
			return Schema{}, nil, nil, fmt.Errorf(
				"compile expression schema: %s %q conflicts with %s",
				"variable",
				variable.Name,
				previous,
			)
		}
		seen[variable.Name] = "another variable"
		variables[variable.Name] = symbol{index: index, kind: variable.Kind}
	}
	for index, function := range schema.Functions {
		validated.Functions[index] = FunctionSpec{
			Name:       function.Name,
			Parameters: append([]Kind(nil), function.Parameters...),
			Result:     function.Result,
		}
		function = validated.Functions[index]
		if err := validateSymbol("function", function.Name, function.Result); err != nil {
			return Schema{}, nil, nil, err
		}
		for parameterIndex, parameter := range function.Parameters {
			if !validKind(parameter) {
				return Schema{}, nil, nil, fmt.Errorf(
					"compile expression schema: function %q parameter %d has invalid type",
					function.Name,
					parameterIndex,
				)
			}
		}
		if previous := seen[function.Name]; previous != "" {
			return Schema{}, nil, nil, fmt.Errorf(
				"compile expression schema: function %q conflicts with %s",
				function.Name,
				previous,
			)
		}
		seen[function.Name] = "another function"
		functions[function.Name] = functionSymbol{index: index, spec: function}
	}
	return validated, variables, functions, nil
}

func validateSymbol(kind, name string, valueKind Kind) error {
	if !validIdentifier(name) || name == "true" || name == "false" {
		return fmt.Errorf(
			"compile expression schema: %s name %q is invalid",
			kind,
			name,
		)
	}
	if !validKind(valueKind) {
		return fmt.Errorf(
			"compile expression schema: %s %q has invalid type",
			kind,
			name,
		)
	}
	return nil
}

func validKind(kind Kind) bool {
	return kind == Boolean || kind == String
}

func validIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, character := range value {
		if character == '_' || unicode.IsLetter(character) ||
			index > 0 && unicode.IsDigit(character) {
			continue
		}
		return false
	}
	return true
}

type tokenKind uint8

const (
	tokenEOF tokenKind = iota
	tokenIdentifier
	tokenString
	tokenTrue
	tokenFalse
	tokenNot
	tokenAnd
	tokenOr
	tokenEqual
	tokenNotEqual
	tokenLeftParenthesis
	tokenRightParenthesis
	tokenComma
)

type expressionToken struct {
	kind   tokenKind
	text   string
	offset int
}

func lex(source string) ([]expressionToken, error) {
	lexer := expressionLexer{source: source}
	result := make([]expressionToken, 0, 16)
	for {
		token, err := lexer.next()
		if err != nil {
			return nil, err
		}
		result = append(result, token)
		if len(result) > maximumTokens {
			return nil, compileError(token.offset, "expression exceeds 512 tokens")
		}
		if token.kind == tokenEOF {
			return result, nil
		}
	}
}

type expressionLexer struct {
	source string
	offset int
}

func (lexer *expressionLexer) next() (expressionToken, error) {
	lexer.skipWhitespace()
	if lexer.offset == len(lexer.source) {
		return expressionToken{kind: tokenEOF, offset: lexer.offset}, nil
	}
	start := lexer.offset
	character, size := utf8.DecodeRuneInString(lexer.source[start:])
	if character == '_' || unicode.IsLetter(character) {
		return lexer.identifierToken(start, size), nil
	}
	if character == '"' {
		return lexer.stringToken(start)
	}
	lexer.offset += size
	return lexer.operatorToken(start, character)
}

func (lexer *expressionLexer) skipWhitespace() {
	for lexer.offset < len(lexer.source) {
		character, size := utf8.DecodeRuneInString(lexer.source[lexer.offset:])
		if !unicode.IsSpace(character) {
			break
		}
		lexer.offset += size
	}
}

func (lexer *expressionLexer) identifierToken(start, size int) expressionToken {
	lexer.offset += size
	for lexer.offset < len(lexer.source) {
		next, nextSize := utf8.DecodeRuneInString(lexer.source[lexer.offset:])
		if next != '_' && !unicode.IsLetter(next) && !unicode.IsDigit(next) {
			break
		}
		lexer.offset += nextSize
	}
	text := lexer.source[start:lexer.offset]
	kind := tokenIdentifier
	switch text {
	case "true":
		kind = tokenTrue
	case "false":
		kind = tokenFalse
	}
	return expressionToken{kind: kind, text: text, offset: start}
}

func (lexer *expressionLexer) operatorToken(
	start int,
	character rune,
) (expressionToken, error) {
	switch character {
	case '(':
		return expressionToken{kind: tokenLeftParenthesis, offset: start}, nil
	case ')':
		return expressionToken{kind: tokenRightParenthesis, offset: start}, nil
	case ',':
		return expressionToken{kind: tokenComma, offset: start}, nil
	case '!':
		if lexer.consume('=') {
			return expressionToken{kind: tokenNotEqual, offset: start}, nil
		}
		return expressionToken{kind: tokenNot, offset: start}, nil
	case '=':
		if lexer.consume('=') {
			return expressionToken{kind: tokenEqual, offset: start}, nil
		}
	case '&':
		if lexer.consume('&') {
			return expressionToken{kind: tokenAnd, offset: start}, nil
		}
	case '|':
		if lexer.consume('|') {
			return expressionToken{kind: tokenOr, offset: start}, nil
		}
	}
	return expressionToken{}, compileError(start, "unexpected token")
}

func (lexer *expressionLexer) consume(expected byte) bool {
	if lexer.offset >= len(lexer.source) || lexer.source[lexer.offset] != expected {
		return false
	}
	lexer.offset++
	return true
}

func (lexer *expressionLexer) stringToken(start int) (expressionToken, error) {
	lexer.offset++
	escaped := false
	for lexer.offset < len(lexer.source) {
		character := lexer.source[lexer.offset]
		lexer.offset++
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			quoted := lexer.source[start:lexer.offset]
			value, err := strconv.Unquote(quoted)
			if err != nil {
				return expressionToken{}, compileError(start, "invalid string literal")
			}
			return expressionToken{kind: tokenString, text: value, offset: start}, nil
		}
		if character < 0x20 {
			return expressionToken{}, compileError(start, "invalid string literal")
		}
	}
	return expressionToken{}, compileError(start, "unterminated string literal")
}

type expressionParser struct {
	tokens    []expressionToken
	position  int
	depth     int
	variables map[string]symbol
	functions map[string]functionSymbol
}

func (parser *expressionParser) parse() (*node, error) {
	result, err := parser.parseOr()
	if err != nil {
		return nil, err
	}
	if token := parser.current(); token.kind != tokenEOF {
		return nil, compileError(token.offset, "unexpected trailing token")
	}
	return result, nil
}

func (parser *expressionParser) parseOr() (*node, error) {
	return parser.parseBinary(parser.parseAnd, tokenOr, operationOr, Boolean)
}

func (parser *expressionParser) parseAnd() (*node, error) {
	return parser.parseBinary(parser.parseEquality, tokenAnd, operationAnd, Boolean)
}

func (parser *expressionParser) parseEquality() (*node, error) {
	left, err := parser.parseUnary()
	if err != nil {
		return nil, err
	}
	token := parser.current()
	if token.kind != tokenEqual && token.kind != tokenNotEqual {
		return left, nil
	}
	parser.position++
	right, err := parser.parseUnary()
	if err != nil {
		return nil, err
	}
	if left.kind != right.kind {
		return nil, compileError(token.offset, "equality operands must have the same type")
	}
	operation := operationEqual
	if token.kind == tokenNotEqual {
		operation = operationNotEqual
	}
	return &node{operation: operation, kind: Boolean, offset: token.offset, left: left, right: right}, nil
}

func (parser *expressionParser) parseUnary() (*node, error) {
	token := parser.current()
	if token.kind != tokenNot {
		return parser.parsePrimary()
	}
	parser.position++
	value, err := parser.parseUnary()
	if err != nil {
		return nil, err
	}
	if value.kind != Boolean {
		return nil, compileError(token.offset, "! requires a Boolean operand")
	}
	return &node{operation: operationNot, kind: Boolean, offset: token.offset, left: value}, nil
}

func (parser *expressionParser) parsePrimary() (*node, error) {
	token := parser.current()
	switch token.kind {
	case tokenTrue, tokenFalse:
		parser.position++
		return &node{operation: operationLiteral, kind: Boolean, offset: token.offset, literal: Bool(token.kind == tokenTrue)}, nil
	case tokenString:
		parser.position++
		return &node{operation: operationLiteral, kind: String, offset: token.offset, literal: Text(token.text)}, nil
	case tokenIdentifier:
		return parser.parseIdentifier(token)
	case tokenLeftParenthesis:
		return parser.parseParenthesized(token)
	case tokenEOF,
		tokenNot,
		tokenAnd,
		tokenOr,
		tokenEqual,
		tokenNotEqual,
		tokenRightParenthesis,
		tokenComma:
		return nil, compileError(token.offset, "expected a value")
	}
	return nil, compileError(token.offset, "token kind is invalid")
}

func (parser *expressionParser) parseIdentifier(token expressionToken) (*node, error) {
	parser.position++
	if parser.current().kind == tokenLeftParenthesis {
		return parser.parseCall(token)
	}
	variable, found := parser.variables[token.text]
	if !found {
		if _, function := parser.functions[token.text]; function {
			return nil, compileError(token.offset, "function requires parentheses")
		}
		return nil, compileError(token.offset, "unknown symbol")
	}
	return &node{operation: operationVariable, kind: variable.kind, offset: token.offset, index: variable.index}, nil
}

func (parser *expressionParser) parseCall(token expressionToken) (*node, error) {
	function, found := parser.functions[token.text]
	if !found {
		return nil, compileError(token.offset, "unknown function")
	}
	parser.position++
	arguments := make([]*node, 0, len(function.spec.Parameters))
	if parser.current().kind != tokenRightParenthesis {
		for {
			argument, err := parser.parseOr()
			if err != nil {
				return nil, err
			}
			arguments = append(arguments, argument)
			if parser.current().kind != tokenComma {
				break
			}
			parser.position++
		}
	}
	if parser.current().kind != tokenRightParenthesis {
		return nil, compileError(parser.current().offset, "function call requires )")
	}
	parser.position++
	if len(arguments) != len(function.spec.Parameters) {
		return nil, compileError(token.offset, "function argument count does not match schema")
	}
	for index, argument := range arguments {
		if argument.kind != function.spec.Parameters[index] {
			return nil, compileError(argument.offset, "function argument type does not match schema")
		}
	}
	return &node{operation: operationCall, kind: function.spec.Result, offset: token.offset, index: function.index, arguments: arguments}, nil
}

func (parser *expressionParser) parseParenthesized(token expressionToken) (*node, error) {
	parser.depth++
	if parser.depth > maximumDepth {
		return nil, compileError(token.offset, "expression nesting exceeds 64 levels")
	}
	defer func() { parser.depth-- }()
	parser.position++
	value, err := parser.parseOr()
	if err != nil {
		return nil, err
	}
	if parser.current().kind != tokenRightParenthesis {
		return nil, compileError(parser.current().offset, "parenthesized expression requires )")
	}
	parser.position++
	return value, nil
}

func (parser *expressionParser) parseBinary(
	next func() (*node, error),
	tokenType tokenKind,
	operation nodeOperation,
	required Kind,
) (*node, error) {
	left, err := next()
	if err != nil {
		return nil, err
	}
	for parser.current().kind == tokenType {
		token := parser.current()
		parser.position++
		right, rightErr := next()
		if rightErr != nil {
			return nil, rightErr
		}
		if left.kind != required || right.kind != required {
			return nil, compileError(token.offset, "logical operator requires Boolean operands")
		}
		left = &node{operation: operation, kind: required, offset: token.offset, left: left, right: right}
	}
	return left, nil
}

func (parser *expressionParser) current() expressionToken {
	if parser.position >= len(parser.tokens) {
		return expressionToken{kind: tokenEOF}
	}
	return parser.tokens[parser.position]
}

func validateInputs(program Program, inputs Inputs) error {
	if len(inputs.Variables) != len(program.variables) {
		return fmt.Errorf(
			"evaluate expression: variable count is %d, want %d",
			len(inputs.Variables),
			len(program.variables),
		)
	}
	for index, variable := range program.variables {
		if inputs.Variables[index].kind != variable.Kind {
			return fmt.Errorf(
				"evaluate expression: variable %q has the wrong type",
				variable.Name,
			)
		}
	}
	if len(inputs.Functions) != len(program.functions) {
		return fmt.Errorf(
			"evaluate expression: function count is %d, want %d",
			len(inputs.Functions),
			len(program.functions),
		)
	}
	for index, function := range inputs.Functions {
		if function == nil {
			return fmt.Errorf(
				"evaluate expression: function %q is nil",
				program.functions[index].Name,
			)
		}
	}
	return nil
}

func evaluateNode(
	ctx context.Context,
	program Program,
	inputs Inputs,
	current *node,
) (Value, error) {
	if err := ctx.Err(); err != nil {
		return Value{}, fmt.Errorf("evaluate expression: %w", err)
	}
	switch current.operation {
	case operationLiteral:
		return current.literal, nil
	case operationVariable:
		return inputs.Variables[current.index], nil
	case operationCall:
		return evaluateCall(ctx, program, inputs, current)
	case operationNot:
		value, err := evaluateNode(ctx, program, inputs, current.left)
		if err != nil {
			return Value{}, err
		}
		boolean, _ := value.BooleanValue()
		return Bool(!boolean), nil
	case operationAnd, operationOr:
		return evaluateLogical(ctx, program, inputs, current)
	case operationEqual, operationNotEqual:
		return evaluateEquality(ctx, program, inputs, current)
	default:
		return Value{}, errors.New("evaluate expression: program operation is invalid")
	}
}

func evaluateCall(
	ctx context.Context,
	program Program,
	inputs Inputs,
	current *node,
) (Value, error) {
	arguments := make([]Value, len(current.arguments))
	for index, argument := range current.arguments {
		value, err := evaluateNode(ctx, program, inputs, argument)
		if err != nil {
			return Value{}, err
		}
		arguments[index] = value
	}
	value, err := inputs.Functions[current.index](ctx, arguments)
	if err != nil {
		return Value{}, fmt.Errorf(
			"evaluate expression function %q: %w",
			program.functions[current.index].Name,
			err,
		)
	}
	if value.kind != program.functions[current.index].Result {
		return Value{}, fmt.Errorf(
			"evaluate expression: function %q returned the wrong type",
			program.functions[current.index].Name,
		)
	}
	return value, nil
}

func evaluateLogical(
	ctx context.Context,
	program Program,
	inputs Inputs,
	current *node,
) (Value, error) {
	left, err := evaluateNode(ctx, program, inputs, current.left)
	if err != nil {
		return Value{}, err
	}
	leftBoolean, _ := left.BooleanValue()
	if current.operation == operationAnd && !leftBoolean {
		return Bool(false), nil
	}
	if current.operation == operationOr && leftBoolean {
		return Bool(true), nil
	}
	right, err := evaluateNode(ctx, program, inputs, current.right)
	if err != nil {
		return Value{}, err
	}
	rightBoolean, _ := right.BooleanValue()
	return Bool(rightBoolean), nil
}

func evaluateEquality(
	ctx context.Context,
	program Program,
	inputs Inputs,
	current *node,
) (Value, error) {
	left, err := evaluateNode(ctx, program, inputs, current.left)
	if err != nil {
		return Value{}, err
	}
	right, err := evaluateNode(ctx, program, inputs, current.right)
	if err != nil {
		return Value{}, err
	}
	equal := left.kind == right.kind && left.boolean == right.boolean && left.text == right.text
	if current.operation == operationNotEqual {
		equal = !equal
	}
	return Bool(equal), nil
}

func compileError(offset int, message string) error {
	return &CompileError{Offset: offset, Message: message}
}
