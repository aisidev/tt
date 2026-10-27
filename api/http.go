package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/aisidev/tt/model"
)

type Route struct {
	Pattern string
	Methods []string
	Handler http.HandlerFunc
}

// Router matches the raw path without ServeMux's slash/dot canonicalization.
// Parameter decoding happens only after method selection, as in Axum.
func Router(routes []Route, middleware ...func(http.Handler) http.Handler) http.Handler {
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Split(r.URL.EscapedPath(), "/")
		for _, route := range routes {
			pattern := strings.Split(route.Pattern, "/")
			if len(path) != len(pattern) {
				continue
			}
			params := map[string]string{}
			match := true
			for i, part := range pattern {
				if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
					if path[i] == "" {
						match = false
						break
					}
					params[part[1:len(part)-1]] = path[i]
				} else if part != path[i] {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			allowed := []string{}
			selected := false
			for _, method := range route.Methods {
				allowed = append(allowed, method)
				selected = selected || r.Method == method
				if method == "GET" {
					allowed = append(allowed, "HEAD")
					selected = selected || r.Method == "HEAD"
				}
			}
			if !selected {
				w.Header().Set("Allow", strings.Join(allowed, ","))
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			for key, value := range params {
				decoded, err := url.PathUnescape(value)
				if err != nil || !utf8.ValidString(decoded) {
					WriteText(w, 400, "Invalid URL: Invalid UTF-8 in `"+key+"`")
					return
				}
				r.SetPathValue(key, decoded)
			}
			route.Handler(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	for i := len(middleware) - 1; i >= 0; i-- {
		handler = middleware[i](handler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Axum's method router attaches Allow after its middleware response, even
		// when authentication rejects an unsupported method with 401.
		path := strings.Split(r.URL.EscapedPath(), "/")
		for _, route := range routes {
			pattern := strings.Split(route.Pattern, "/")
			if len(pattern) != len(path) {
				continue
			}
			match := true
			for i, part := range pattern {
				if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
					match = match && path[i] != ""
				} else {
					match = match && part == path[i]
				}
			}
			if !match {
				continue
			}
			selected := false
			allowed := []string{}
			for _, method := range route.Methods {
				allowed = append(allowed, method)
				selected = selected || method == r.Method
				if method == "GET" {
					allowed = append(allowed, "HEAD")
					selected = selected || r.Method == "HEAD"
				}
			}
			if !selected {
				w.Header().Set("Allow", strings.Join(allowed, ","))
			}
			break
		}
		handler.ServeHTTP(w, r)
	})
}

type headResponse struct {
	http.ResponseWriter
	status int
	size   int
}

func (w *headResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *headResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	w.size += len(data)
	return len(data), nil
}
func StripHeadBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" {
			next.ServeHTTP(w, r)
			return
		}
		head := &headResponse{ResponseWriter: w}
		next.ServeHTTP(head, r)
		if head.Header().Get("Content-Length") == "" {
			head.Header().Set("Content-Length", strconv.Itoa(head.size))
		}
		if head.status == 0 {
			head.status = 200
		}
		w.WriteHeader(head.status)
	})
}
func WriteJSON(w http.ResponseWriter, status int, value any) {
	data, err := model.SerdeJSON(value)
	if err != nil {
		WriteText(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
func WriteText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(text)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

const JSONBodyLimit = 2 * 1024 * 1024

func ReadJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var value T
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	parts := strings.SplitN(media, "/", 2)
	if err != nil || len(parts) != 2 || parts[0] != "application" || (parts[1] != "json" && !strings.HasSuffix(parts[1], "+json")) {
		WriteText(w, 415, "Expected request with `Content-Type: application/json`")
		return value, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, JSONBodyLimit+1))
	if err != nil {
		WriteText(w, 400, "Failed to buffer the request body: "+err.Error())
		return value, false
	}
	if len(data) > JSONBodyLimit {
		WriteText(w, 413, "Failed to buffer the request body: length limit exceeded")
		return value, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if reason, offset, status, ok := earlyDataDiagnostic(data, reflect.TypeFor[T](), "", 0); ok {
			writeJSONRejection(w, data, status, reason, offset)
			return value, false
		}
		reason, offset := syntaxDiagnostic(data, err)
		writeJSONRejection(w, data, 400, reason, offset)
		return value, false
	}
	start := bytes.Index(data, raw)
	if err := json.Unmarshal(raw, &value); err != nil {
		reason, offset, status := dataDiagnostic(raw, reflect.TypeFor[T](), err, "", start)
		writeJSONRejection(w, data, status, reason, offset)
		return value, false
	}
	end := int(decoder.InputOffset())
	for end < len(data) && (data[end] == ' ' || data[end] == '\t' || data[end] == '\r' || data[end] == '\n') {
		end++
	}
	if end < len(data) {
		writeJSONRejection(w, data, 400, "trailing characters", end+1)
		return value, false
	}
	return value, true
}
func writeJSONRejection(w http.ResponseWriter, data []byte, status int, reason string, offset int) {
	offset = max(0, min(offset, len(data)))
	line := 1
	column := 0
	for _, c := range data[:offset] {
		if c == '\n' {
			line++
			column = 0
		} else {
			column++
		}
	}
	prefix := "Failed to deserialize the JSON body into the target type: "
	if status == 400 {
		prefix = "Failed to parse the request body as JSON: "
	}
	WriteText(w, status, fmt.Sprintf("%s%s at line %d column %d", prefix, reason, line, column))
}

// HeaderText matches HeaderValue::to_str for backup precondition headers.
func HeaderText(headers http.Header, name string) *string {
	values := headers.Values(name)
	if len(values) == 0 {
		return nil
	}
	value := values[0]
	for _, b := range []byte(value) {
		if b < 0x20 || b > 0x7e {
			return nil
		}
	}
	return &value
}
