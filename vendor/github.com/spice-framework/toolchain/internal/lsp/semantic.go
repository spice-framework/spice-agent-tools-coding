package lsp

import (
	"bytes"
	"unicode"
	"unicode/utf8"
)

const (
	semanticDecoratorToken = iota
	semanticParameterToken
	semanticStringToken
	semanticNumberToken
	semanticKeywordToken
	semanticOperatorToken
)

type semanticTokensParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type semanticTokensResult struct {
	Data []uint32 `json:"data"`
}

type semanticToken struct {
	line   int
	start  int
	length int
	kind   int
}

type annotationSemanticStep struct {
	token semanticToken
	next  int
	emit  bool
	stop  bool
}

func (server *Server) semanticTokens(message rpcMessage) error {
	if !message.request() {
		return nil
	}
	var params semanticTokensParams
	if err := decodeParams(message.Params, &params); err != nil {
		return server.writer.failure(
			message.ID,
			invalidParamsCode,
			err.Error(),
		)
	}
	source, _, found := server.featureSnapshot(params.TextDocument.URI)
	if !found {
		return server.writer.response(
			message.ID,
			semanticTokensResult{Data: []uint32{}},
		)
	}
	return server.writer.response(
		message.ID,
		semanticTokensResult{Data: encodeSemanticTokens(
			annotationSemanticTokens(source.content),
		)},
	)
}

func annotationSemanticTokens(content []byte) []semanticToken {
	var tokens []semanticToken
	for line, start := 0, 0; start <= len(content); line++ {
		end := bytes.IndexByte(content[start:], '\n')
		last := end < 0
		if last {
			end = len(content)
		} else {
			end += start
		}
		lineContent := bytes.TrimSuffix(content[start:end], []byte{'\r'})
		tokens = append(tokens, annotationLineSemanticTokens(
			lineContent,
			line,
		)...)
		if last {
			break
		}
		start = end + 1
	}
	return tokens
}

func annotationSemanticToken(
	lineContent []byte,
	line int,
) (semanticToken, bool) {
	start, end, found := annotationNameBounds(lineContent)
	if !found {
		return semanticToken{}, false
	}
	return semanticTokenAt(lineContent, line, start, end, semanticDecoratorToken), true
}

func annotationNameBounds(lineContent []byte) (int, int, bool) {
	offset := skipHorizontalWhitespace(lineContent, 0)
	if !bytes.HasPrefix(lineContent[offset:], []byte("//")) {
		return 0, 0, false
	}
	offset = skipHorizontalWhitespace(lineContent, offset+2)
	if offset >= len(lineContent) || lineContent[offset] != '@' {
		return 0, 0, false
	}
	end := annotationNameEnd(lineContent, offset+1)
	if end == offset+1 {
		return 0, 0, false
	}
	return offset, end, true
}

func annotationLineSemanticTokens(
	lineContent []byte,
	line int,
) []semanticToken {
	nameStart, nameEnd, found := annotationNameBounds(lineContent)
	if !found {
		return nil
	}
	result := []semanticToken{
		semanticTokenAt(
			lineContent,
			line,
			nameStart,
			nameEnd,
			semanticDecoratorToken,
		),
	}
	offset := skipHorizontalWhitespace(lineContent, nameEnd)
	if offset >= len(lineContent) || lineContent[offset] != '(' {
		return result
	}
	for offset < len(lineContent) {
		offset = skipHorizontalWhitespace(lineContent, offset)
		if offset >= len(lineContent) {
			break
		}
		step := nextAnnotationSemanticStep(lineContent, line, offset)
		if step.emit {
			result = append(result, step.token)
		}
		if step.stop {
			return result
		}
		offset = step.next
	}
	return result
}

func nextAnnotationSemanticStep(
	content []byte,
	line int,
	offset int,
) annotationSemanticStep {
	switch {
	case content[offset] == '"':
		end := quotedStringEnd(content, offset)
		return annotationTokenStep(
			content,
			line,
			offset,
			end,
			semanticStringToken,
		)
	case isNumberStart(content, offset):
		end := numberEnd(content, offset)
		return annotationTokenStep(
			content,
			line,
			offset,
			end,
			semanticNumberToken,
		)
	case annotationIdentifierStart(content[offset]):
		end := annotationIdentifierEnd(content, offset)
		kind := semanticKeywordToken
		if nextNonSpace(content, end) == '=' {
			kind = semanticParameterToken
		}
		return annotationTokenStep(content, line, offset, end, kind)
	case isAnnotationOperator(content[offset]):
		return annotationSemanticStep{
			token: semanticTokenAt(
				content,
				line,
				offset,
				offset+1,
				semanticOperatorToken,
			),
			next: offset + 1,
			emit: true,
			stop: content[offset] == ')',
		}
	default:
		_, size := utf8.DecodeRune(content[offset:])
		return annotationSemanticStep{next: offset + max(size, 1)}
	}
}

func annotationTokenStep(
	content []byte,
	line int,
	start int,
	end int,
	kind int,
) annotationSemanticStep {
	return annotationSemanticStep{
		token: semanticTokenAt(content, line, start, end, kind),
		next:  end,
		emit:  true,
	}
}

func semanticTokenAt(
	lineContent []byte,
	line int,
	start int,
	end int,
	kind int,
) semanticToken {
	return semanticToken{
		line:   line,
		start:  utf16Length(lineContent[:start]),
		length: utf16Length(lineContent[start:end]),
		kind:   kind,
	}
}

func quotedStringEnd(content []byte, start int) int {
	escaped := false
	for offset := start + 1; offset < len(content); offset++ {
		switch {
		case escaped:
			escaped = false
		case content[offset] == '\\':
			escaped = true
		case content[offset] == '"':
			return offset + 1
		}
	}
	return len(content)
}

func numberEnd(content []byte, start int) int {
	end := start
	if content[end] == '-' {
		end++
	}
	for end < len(content) && isASCIIDigit(content[end]) {
		end++
	}
	return end
}

func isNumberStart(content []byte, offset int) bool {
	return isASCIIDigit(content[offset]) ||
		content[offset] == '-' &&
			offset+1 < len(content) &&
			isASCIIDigit(content[offset+1])
}

func annotationIdentifierEnd(content []byte, start int) int {
	end := start
	for end < len(content) {
		character, size := utf8.DecodeRune(content[end:])
		if character == utf8.RuneError && size == 1 {
			break
		}
		if character != '_' && !isAnnotationIdentifierCharacter(character) {
			break
		}
		end += size
	}
	return end
}

func annotationIdentifierStart(value byte) bool {
	return value == '_' ||
		value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z'
}

func nextNonSpace(content []byte, start int) byte {
	offset := skipHorizontalWhitespace(content, start)
	if offset >= len(content) {
		return 0
	}
	return content[offset]
}

func isASCIIDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func isAnnotationOperator(value byte) bool {
	switch value {
	case '=', '(', ')', '[', ']', ',':
		return true
	default:
		return false
	}
}

func skipHorizontalWhitespace(content []byte, offset int) int {
	for offset < len(content) &&
		(content[offset] == ' ' || content[offset] == '\t') {
		offset++
	}
	return offset
}

func annotationNameEnd(content []byte, start int) int {
	end := start
	for end < len(content) {
		character, size := utf8.DecodeRune(content[end:])
		if character == utf8.RuneError && size == 1 {
			break
		}
		if character != '.' &&
			character != '_' &&
			!isAnnotationIdentifierCharacter(character) {
			break
		}
		end += size
	}
	return end
}

func isAnnotationIdentifierCharacter(character rune) bool {
	return unicode.IsLetter(character) || unicode.IsDigit(character)
}

func encodeSemanticTokens(tokens []semanticToken) []uint32 {
	data := make([]uint32, 0, len(tokens)*5)
	previousLine := 0
	previousStart := 0
	for _, token := range tokens {
		deltaLine := token.line - previousLine
		deltaStart := token.start
		if deltaLine == 0 {
			deltaStart -= previousStart
		}
		data = append(
			data,
			uint32(deltaLine),    // #nosec G115 -- source positions are non-negative and document-bounded.
			uint32(deltaStart),   // #nosec G115 -- source positions are non-negative and document-bounded.
			uint32(token.length), // #nosec G115 -- token lengths are document-bounded.
			uint32(token.kind),   // #nosec G115 -- token kinds are fixed non-negative constants.
			0,
		)
		previousLine = token.line
		previousStart = token.start
	}
	return data
}
