package main

import (
	"bytes"
	"strconv"
	"strings"
)

// httpRequestSplitter incrementally frames complete HTTP requests out of a raw
// TCP byte stream. It is used by context-based routing to know where one
// request ends and the next begins, so the request's size can be measured
// before any of its bytes are committed to a backend service.
//
// Framing rules:
//   - Requests with Content-Length complete when the full body has arrived.
//   - Requests with Transfer-Encoding: chunked complete after the terminal
//     chunk (and optional trailers).
//   - Bodyless methods (GET, HEAD, ...) complete at the end of the headers.
//   - Anything whose framing cannot be determined (methods with a body but no
//     length, Expect: 100-continue, connection upgrades, non-HTTP traffic)
//     switches the splitter into passthrough: all remaining bytes are handed
//     back as-is and no further framing is attempted.
//
// Byte slices returned by Write and FlushIncomplete alias internal buffers and
// are only valid until the next call on the splitter.
type httpRequestSplitter struct {
	buffer      []byte
	consumed    int // offset in buffer where the in-flight request starts
	state       splitterFramingState
	passthrough bool
	bodyStart   int   // offset where the current request's body starts
	bodyRemain  int64 // content-length or current chunk bytes still expected
	cursor      int   // chunked parsing position
}

type splitterFramingState int

const (
	framingHeaders splitterFramingState = iota
	framingBody
	framingChunkSize
	framingChunkData
	framingTrailers
)

type httpRequestSplitResult struct {
	completedRequests  [][]byte
	enteredPassthrough bool
	// throughBytes is set when enteredPassthrough: all received bytes that are
	// not part of a completed request, to be forwarded as-is.
	throughBytes []byte
}

// Methods likely to be seen on LLM service ports. A stream that does not start
// with one of these (as a prefix) is treated as non-HTTP traffic.
var httpMethods = []string{
	"OPTIONS", "GET", "HEAD", "POST", "PUT", "DELETE", "TRACE", "CONNECT", "PATCH",
	"PROPFIND", "PROPPATCH", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", "REPORT",
	"MKCALENDAR", "ACL", "SEARCH",
}

func newHttpRequestSplitter() *httpRequestSplitter {
	return &httpRequestSplitter{state: framingHeaders}
}

func (s *httpRequestSplitter) Write(chunk []byte) httpRequestSplitResult {
	if s.passthrough {
		return httpRequestSplitResult{enteredPassthrough: true, throughBytes: chunk}
	}

	if s.consumed > 0 {
		// Compact completed requests out of the buffer. Callers consume
		// completed requests before calling Write again (documented contract),
		// so mutating the consumed region is safe and keeps memory bounded on
		// long-lived keep-alive connections.
		s.buffer = append(s.buffer[:0], s.buffer[s.consumed:]...)
		s.bodyStart -= s.consumed
		s.cursor -= s.consumed
		s.consumed = 0
	}
	s.buffer = append(s.buffer, chunk...)

	var result httpRequestSplitResult
	if s.parse(&result) {
		s.passthrough = true
		result.enteredPassthrough = true
		result.throughBytes = s.buffer[s.consumed:]
		s.buffer = nil
	}
	return result
}

// FlushIncomplete returns the bytes of the currently in-flight (not yet fully
// received) request, for forwarding when no more bytes can arrive.
func (s *httpRequestSplitter) FlushIncomplete() []byte {
	if s.passthrough {
		return nil
	}
	return s.buffer[s.consumed:]
}

// parse consumes as many complete requests from the buffer as possible,
// appending them to result.completedRequests. It returns true when framing
// must be abandoned (passthrough).
func (s *httpRequestSplitter) parse(result *httpRequestSplitResult) bool {
	for {
		if s.consumed == len(s.buffer) {
			return false // need more bytes
		}
		switch s.state {
		case framingHeaders:
			if !startsWithHttpMethodPrefix(s.buffer[s.consumed:]) {
				return true // not HTTP traffic
			}
			headerEnd, terminatorLength := findHeaderTerminator(s.buffer[s.consumed:])
			if headerEnd < 0 {
				return false
			}
			headerEnd += s.consumed
			headersEndAbsolute := headerEnd + terminatorLength
			method, headers, ok := parseHttpHead(s.buffer[s.consumed:headersEndAbsolute])
			if !ok {
				return true
			}
			if hasHeaderValueToken(headers, "expect", "100-continue") {
				// The client waits for a 100 Continue that only the backend can
				// send, so the body cannot be counted here.
				return true
			}
			if method == "CONNECT" {
				return true
			}
			if hasHeaderValueToken(headers, "connection", "upgrade") || headers["upgrade"] != "" {
				result.completedRequests = append(result.completedRequests, s.buffer[s.consumed:headersEndAbsolute])
				s.consumed = headersEndAbsolute
				return true // the upgraded stream after the headers cannot be framed
			}
			if hasHeaderValueToken(headers, "transfer-encoding", "chunked") {
				s.bodyStart = headersEndAbsolute
				s.cursor = headersEndAbsolute
				s.state = framingChunkSize
				continue
			}
			if contentLength, present := headers["content-length"]; present {
				length, err := strconv.ParseInt(contentLength, 10, 64)
				if err != nil || length < 0 {
					return true
				}
				s.bodyStart = headersEndAbsolute
				s.bodyRemain = length
				s.state = framingBody
				continue
			}
			switch method {
			case "GET", "HEAD", "DELETE", "OPTIONS", "TRACE":
				result.completedRequests = append(result.completedRequests, s.buffer[s.consumed:headersEndAbsolute])
				s.consumed = headersEndAbsolute
				continue // possibly pipelined requests follow
			default:
				// A request that usually carries a body but declares no length:
				// its end cannot be determined without consuming it.
				return true
			}
		case framingBody:
			if int64(len(s.buffer)-s.bodyStart) >= s.bodyRemain {
				bodyEnd := s.bodyStart + int(s.bodyRemain)
				result.completedRequests = append(result.completedRequests, s.buffer[s.consumed:bodyEnd])
				s.consumed = bodyEnd
				s.state = framingHeaders
				continue
			}
			return false
		case framingChunkSize:
			line, next, ok := readLine(s.buffer, s.cursor)
			if !ok {
				return false
			}
			chunkSize, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(line, ";", 2)[0]), 16, 63)
			if err != nil || chunkSize < 0 {
				return true
			}
			s.cursor = next
			if chunkSize == 0 {
				s.state = framingTrailers
				continue
			}
			s.bodyRemain = chunkSize
			s.state = framingChunkData
		case framingChunkData:
			// chunk bytes plus the trailing CRLF
			if int64(len(s.buffer)-s.cursor) >= s.bodyRemain+2 {
				s.cursor += int(s.bodyRemain) + 2
				s.state = framingChunkSize
				continue
			}
			return false
		case framingTrailers:
			// Trailer section: header lines until an empty line
			for {
				line, next, ok := readLine(s.buffer, s.cursor)
				if !ok {
					return false
				}
				s.cursor = next
				if line == "" {
					result.completedRequests = append(result.completedRequests, s.buffer[s.consumed:s.cursor])
					s.consumed = s.cursor
					s.state = framingHeaders
					break
				}
			}
		}
	}
}

// startsWithHttpMethodPrefix reports whether data is still consistent with the
// beginning of an HTTP request (a known method followed by a space).
func startsWithHttpMethodPrefix(data []byte) bool {
	for _, method := range httpMethods {
		prefix := []byte(method + " ")
		if len(data) <= len(prefix) && bytes.HasPrefix(prefix, data) {
			return true // data is a prefix of "METHOD "
		}
		if bytes.HasPrefix(data, prefix) {
			return true // data starts with "METHOD "
		}
	}
	return false
}

// findHeaderTerminator finds the earliest end-of-headers marker (CRLFCRLF or,
// leniently like net/textproto, LFLF) in data, returning the offset where the
// header block ends and the length of the terminator.
func findHeaderTerminator(data []byte) (int, int) {
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	lflf := bytes.Index(data, []byte("\n\n"))
	switch {
	case crlf >= 0 && (lflf < 0 || crlf <= lflf):
		return crlf, 4
	case lflf >= 0:
		return lflf, 2
	default:
		return -1, 0
	}
}

// parseHttpHead parses the request line and headers of one HTTP head block
// (headers already stripped of the terminator).
func parseHttpHead(head []byte) (string, map[string]string, bool) {
	lines := strings.Split(string(head), "\n")
	requestLineParts := strings.Fields(strings.TrimSuffix(lines[0], "\r"))
	if len(requestLineParts) < 3 || !strings.HasPrefix(requestLineParts[len(requestLineParts)-1], "HTTP/") {
		return "", nil, false
	}
	method := requestLineParts[0]
	headers := make(map[string]string)
	for _, line := range lines[1:] {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		colonIndex := strings.IndexByte(line, ':')
		if colonIndex < 0 {
			return "", nil, false
		}
		key := strings.ToLower(strings.TrimSpace(line[:colonIndex]))
		value := strings.TrimSpace(line[colonIndex+1:])
		if existing, present := headers[key]; present {
			headers[key] = existing + ", " + value
		} else {
			headers[key] = value
		}
	}
	return method, headers, true
}

func hasHeaderValueToken(headers map[string]string, name string, expectedToken string) bool {
	value, present := headers[name]
	if !present {
		return false
	}
	for _, token := range strings.Split(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), expectedToken) {
			return true
		}
	}
	return false
}

// readLine reads one line (terminated by \n, with an optional preceding \r
// stripped) starting at pos. ok is false when no complete line is buffered yet.
func readLine(data []byte, pos int) (line string, next int, ok bool) {
	newlineIndex := bytes.IndexByte(data[pos:], '\n')
	if newlineIndex < 0 {
		return "", 0, false
	}
	lineEnd := pos + newlineIndex
	line = string(data[pos:lineEnd])
	line = strings.TrimSuffix(line, "\r")
	return line, lineEnd + 1, true
}
