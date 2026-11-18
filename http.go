// Raw HTTP/1.1 server (hand-rolled: no net/http) and the lifecycle
// client (register / done) — mirrors the Node http server's behaviour
// closely enough for the suite's fetch-based clients.
package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var statusPhrases = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

type header struct {
	name  string
	value string
}

type request struct {
	method  string
	target  string
	proto   string // "HTTP/1.1" etc
	path    string // raw path before '?'
	query   string // raw query string after '?'
	headers map[string]string
	body    []byte
	// parsed query, lazily
	queryParams []queryParam
	keepAlive   bool
	// path segments: split on '/', empties dropped, each decodeURIComponent'd
	// (raw segment kept on decode error) — mirrors the TS router.
	segments []string
}

type queryParam struct {
	key, value string
}

func (rq *request) queryGet(name string) (string, bool) {
	rq.parseQuery()
	for _, p := range rq.queryParams {
		if p.key == name {
			return p.value, true
		}
	}
	return "", false
}

func (rq *request) parseQuery() {
	if rq.queryParams != nil {
		return
	}
	rq.queryParams = []queryParam{}
	q := rq.query
	for len(q) > 0 {
		amp := strings.IndexByte(q, '&')
		var item string
		if amp < 0 {
			item = q
			q = ""
		} else {
			item = q[:amp]
			q = q[amp+1:]
		}
		if item == "" {
			continue
		}
		key, value := item, ""
		if eq := strings.IndexByte(item, '='); eq >= 0 {
			key = item[:eq]
			value = item[eq+1:]
		}
		rq.queryParams = append(rq.queryParams, queryParam{
			key:   urlQueryUnescape(key),
			value: urlQueryUnescape(value),
		})
	}
}

// querySet mirrors URLSearchParams.set: replace first occurrence, drop the
// rest, or append.
func (rq *request) querySet(name, value string) {
	rq.parseQuery()
	found := -1
	out := rq.queryParams[:0]
	for _, p := range rq.queryParams {
		if p.key == name {
			if found < 0 {
				found = len(out)
				out = append(out, queryParam{key: name, value: value})
			}
			continue
		}
		out = append(out, p)
	}
	if found < 0 {
		out = append(out, queryParam{key: name, value: value})
	}
	rq.queryParams = out
}

func (rq *request) queryString() string {
	rq.parseQuery()
	var sb strings.Builder
	for i, p := range rq.queryParams {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(urlQueryEscape(p.key))
		sb.WriteByte('=')
		sb.WriteString(urlQueryEscape(p.value))
	}
	return sb.String()
}

// urlQueryUnescape: '+' -> space, %XX decoded, invalid sequences -> U+FFFD.
func urlQueryUnescape(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	var out []byte
	for i := 0; i < len(s); {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out = append(out, hexVal(s[i+1])*16+hexVal(s[i+2]))
			i += 3
			continue
		}
		if s[i] == '%' {
			out = append(out, 0xEF, 0xBF, 0xBD)
			i++
			continue
		}
		out = append(out, s[i])
		i++
	}
	return string(out)
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// urlQueryEscape mirrors URLSearchParams.toString encoding.
func urlQueryEscape(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '_' || c == '.' || c == '*' {
			sb.WriteByte(c)
		} else if c == ' ' {
			sb.WriteByte('+')
		} else {
			const hexd = "0123456789ABCDEF"
			sb.WriteByte('%')
			sb.WriteByte(hexd[c>>4])
			sb.WriteByte(hexd[c&0xf])
		}
	}
	return sb.String()
}

// decodeURIComponent: percent-decode then strict UTF-8; on any error the
// caller keeps the raw segment.
func decodeURIComponent(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, true
	}
	var raw []byte
	for i := 0; i < len(s); {
		if s[i] == '%' {
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return "", false
			}
			raw = append(raw, hexVal(s[i+1])*16+hexVal(s[i+2]))
			i += 3
			continue
		}
		raw = append(raw, s[i])
		i++
	}
	if !utf8Valid(raw) {
		return "", false
	}
	return string(raw), true
}

func utf8Valid(b []byte) bool {
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			i++
			continue
		}
		var size int
		switch {
		case c >= 0xC2 && c <= 0xDF:
			size = 2
		case c >= 0xE0 && c <= 0xEF:
			size = 3
		case c >= 0xF0 && c <= 0xF4:
			size = 4
		default:
			return false
		}
		if i+size > len(b) {
			return false
		}
		for j := 1; j < size; j++ {
			if b[i+j]&0xC0 != 0x80 {
				return false
			}
		}
		// Overlong / surrogate / > U+10FFFF checks.
		switch size {
		case 2:
			if b[i] == 0xC0 || b[i] == 0xC1 {
				return false
			}
		case 3:
			if b[i] == 0xE0 && b[i+1] < 0xA0 {
				return false
			}
			if b[i] == 0xED && b[i+1] >= 0xA0 {
				return false
			}
		case 4:
			if b[i] == 0xF0 && b[i+1] < 0x90 {
				return false
			}
			if b[i] == 0xF4 && b[i+1] >= 0x90 {
				return false
			}
		}
		i += size
	}
	return true
}

type response struct {
	status  int
	headers []header
	body    []byte
}

var reqMu sync.Mutex // serialises handler execution (Node event-loop semantics)
var requestCounter int64

func nextReqID() string {
	n := atomic.AddInt64(&requestCounter, 1)
	return "req-" + strconv.FormatInt(n, 10)
}

func jsonResp(status int, value *JVal, extra ...header) *response {
	body := encodeJSON(value)
	h := []header{{"content-type", "application/json"}, {"x-github-request-id", nextReqID()}}
	h = append(h, extra...)
	return &response{status: status, headers: h, body: body}
}

func errResp(status int, message string) *response {
	return jsonResp(status, JObjV(JF("message", JStrV(message))))
}

func gitResp(status int, contentType string, body []byte) *response {
	return &response{
		status:  status,
		headers: []header{{"content-type", contentType}, {"x-github-request-id", nextReqID()}},
		body:    body,
	}
}

func emptyResp(status int) *response {
	return &response{
		status:  status,
		headers: []header{{"x-github-request-id", nextReqID()}},
		body:    nil,
	}
}

// serialize writes the response to w.
func (r *response) serialize(w *bufio.Writer) {
	phrase := statusPhrases[r.status]
	if phrase == "" {
		phrase = "Unknown"
	}
	var sb strings.Builder
	sb.WriteString("HTTP/1.1 ")
	sb.WriteString(strconv.Itoa(r.status))
	sb.WriteByte(' ')
	sb.WriteString(phrase)
	sb.WriteString("\r\n")
	for _, h := range r.headers {
		sb.WriteString(h.name)
		sb.WriteString(": ")
		sb.WriteString(h.value)
		sb.WriteString("\r\n")
	}
	sb.WriteString("Date: ")
	sb.WriteString(time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	sb.WriteString("\r\nContent-Length: ")
	sb.WriteString(strconv.Itoa(len(r.body)))
	sb.WriteString("\r\n\r\n")
	w.WriteString(sb.String())
	if len(r.body) > 0 {
		w.Write(r.body)
	}
}

// --- Connection handling ---

func serveConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 8192)
	bw := bufio.NewWriterSize(conn, 8192)
	for {
		rq, err := readRequest(br, bw)
		if err != nil {
			return
		}
		var resp *response
		reqMu.Lock()
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					fmt.Fprintf(os.Stderr, "[git-forge] handler panic: %v\n", rec)
					resp = errResp(500, "Internal Server Error")
				}
			}()
			resp = route(rq)
		}()
		reqMu.Unlock()
		if resp == nil {
			resp = errResp(500, "Internal Server Error")
		}
		resp.serialize(bw)
		if err := bw.Flush(); err != nil {
			return
		}
		if !rq.keepAlive {
			return
		}
	}
}

func readRequest(br *bufio.Reader, bw *bufio.Writer) (*request, error) {
	line, err := readLine(br, 65536)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, errBadRequest
	}
	rq := &request{
		method:  parts[0],
		target:  parts[1],
		proto:   parts[2],
		headers: map[string]string{},
	}
	// Absolute-form target: strip scheme://authority.
	target := rq.target
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		rest := target[strings.Index(target, "://")+3:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			target = rest[slash:]
		} else {
			target = "/"
		}
	}
	if qi := strings.IndexByte(target, '?'); qi >= 0 {
		rq.path = target[:qi]
		rq.query = target[qi+1:]
	} else {
		rq.path = target
	}
	// Segments: split on '/', drop empties, decodeURIComponent each.
	for _, seg := range strings.Split(rq.path, "/") {
		if seg == "" {
			continue
		}
		if dec, ok := decodeURIComponent(seg); ok {
			rq.segments = append(rq.segments, dec)
		} else {
			rq.segments = append(rq.segments, seg)
		}
	}
	// Headers.
	for {
		hline, err := readLine(br, 65536)
		if err != nil {
			return nil, err
		}
		if hline == "" {
			break
		}
		colon := strings.IndexByte(hline, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(strings.Trim(hline[:colon], " \t"))
		value := strings.Trim(hline[colon+1:], " \t")
		if prev, ok := rq.headers[name]; ok {
			rq.headers[name] = prev + ", " + value
		} else {
			rq.headers[name] = value
		}
	}
	// Expect: 100-continue: answer the interim response before reading the body.
	if strings.EqualFold(rq.headers["expect"], "100-continue") {
		bw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
		if err := bw.Flush(); err != nil {
			return nil, err
		}
	}
	// Body.
	if te := rq.headers["transfer-encoding"]; strings.Contains(strings.ToLower(te), "chunked") {
		body, err := readChunked(br)
		if err != nil {
			return nil, err
		}
		rq.body = body
	} else if cl := rq.headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 || n > 64*1024*1024 {
			return nil, errBadRequest
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, err
		}
		rq.body = body
	}
	// Keep-alive.
	if rq.proto == "HTTP/1.0" {
		rq.keepAlive = strings.Contains(strings.ToLower(rq.headers["connection"]), "keep-alive")
	} else {
		rq.keepAlive = !strings.Contains(strings.ToLower(rq.headers["connection"]), "close")
	}
	return rq, nil
}

var errBadRequest = fmt.Errorf("bad request")

func readLine(br *bufio.Reader, maxLen int) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	if len(line) > maxLen {
		return "", errBadRequest
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		line, err := readLine(br, 4096)
		if err != nil {
			return nil, err
		}
		if semi := strings.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || size < 0 || size > 64*1024*1024 {
			return nil, errBadRequest
		}
		if size == 0 {
			// Trailers until an empty line.
			for {
				tline, err := readLine(br, 4096)
				if err != nil {
					return nil, err
				}
				if tline == "" {
					break
				}
			}
			return body, nil
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, err
		}
		body = append(body, chunk...)
		// Trailing CRLF.
		crlf := make([]byte, 2)
		if _, err := io.ReadFull(br, crlf); err != nil {
			return nil, err
		}
	}
}

// --- Lifecycle client (register / done) ---

// httpDo performs one minimal HTTP/1.1 request over a fresh connection.
func httpDo(method, url, body string) (int, []byte, error) {
	hostport, path := splitHTTPURL(url)
	if hostport == "" {
		return 0, nil, fmt.Errorf("bad url %q", url)
	}
	conn, err := net.DialTimeout("tcp", hostport, 30*time.Second)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var sb strings.Builder
	sb.WriteString(method)
	sb.WriteByte(' ')
	sb.WriteString(path)
	sb.WriteString(" HTTP/1.1\r\nHost: ")
	sb.WriteString(hostport)
	sb.WriteString("\r\n")
	if body != "" {
		sb.WriteString("Content-Type: application/json\r\nContent-Length: ")
		sb.WriteString(strconv.Itoa(len(body)))
		sb.WriteString("\r\n")
	}
	sb.WriteString("Connection: close\r\n\r\n")
	sb.WriteString(body)
	if _, err := io.WriteString(conn, sb.String()); err != nil {
		return 0, nil, err
	}
	br := bufio.NewReader(conn)
	statusLine, err := readLine(br, 4096)
	if err != nil {
		return 0, nil, err
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return 0, nil, fmt.Errorf("bad status line %q", statusLine)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, nil, err
	}
	headers := map[string]string{}
	for {
		hline, err := readLine(br, 4096)
		if err != nil {
			return 0, nil, err
		}
		if hline == "" {
			break
		}
		colon := strings.IndexByte(hline, ':')
		if colon < 0 {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(hline[:colon]))] = strings.TrimSpace(hline[colon+1:])
	}
	var respBody []byte
	if cl := headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(cl)
		if err == nil && n > 0 {
			respBody = make([]byte, n)
			_, err = io.ReadFull(br, respBody)
			if err != nil {
				return 0, nil, err
			}
		}
	} else {
		respBody, _ = io.ReadAll(br)
	}
	return status, respBody, nil
}

func splitHTTPURL(url string) (hostport, path string) {
	rest := url
	if strings.HasPrefix(rest, "http://") {
		rest = rest[len("http://"):]
	} else if strings.HasPrefix(rest, "https://") {
		rest = rest[len("https://"):]
	} else {
		return "", ""
	}
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return rest[:slash], rest[slash:]
	}
	return rest, "/"
}

func retryDelayMs(attempt int) int {
	d := 250
	for i := 0; i < attempt && i < 8; i++ {
		d *= 2
	}
	if d > 2000 {
		d = 2000
	}
	return d
}

func registerWithEnv(envURL string, port int) error {
	body := `{"port":` + strconv.Itoa(port) + `}`
	for attempt := 0; ; attempt++ {
		status, respBody, err := httpDo("POST", envURL+"/register", body)
		if err == nil {
			if status == 200 || status == 409 {
				return nil
			}
			if status == 400 || status == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", status, string(respBody))
			}
		}
		time.Sleep(time.Duration(retryDelayMs(attempt)) * time.Millisecond)
	}
}

func waitForDone(envURL string) error {
	for attempt := 0; ; attempt++ {
		status, respBody, err := httpDo("GET", envURL+"/done", "")
		if err == nil && status >= 200 && status < 300 {
			if v, perr := parseJSON(respBody); perr == nil {
				if done := v.ObjGet("done"); done != nil && done.Kind == JBool && done.Bool {
					return nil
				}
			}
		}
		time.Sleep(time.Duration(retryDelayMs(attempt)) * time.Millisecond)
	}
}

var _ = bytes.MinRead
