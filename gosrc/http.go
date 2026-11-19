package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

const jsonType = "application/json; charset=utf-8"

// jsonReply writes a JSON reply with the mandatory non-empty X-GitHub-Request-Id.
func jsonReply(w http.ResponseWriter, status int, body any, extra map[string]string) {
	text, err := json.Marshal(body)
	if err != nil {
		text = []byte(`{"message":"marshal error"}`)
	}
	h := w.Header()
	h.Set("Content-Type", jsonType)
	h.Set("Content-Length", strconv.Itoa(len(text)))
	h.Set("X-GitHub-Request-Id", nextRequestId())
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(text)
}

func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(r.Body)
	return b
}

type Ctx struct {
	Method   string
	Pathname string
	Query    url.Values
	RawBody  []byte
	Authed   bool
	Host     string
	Header   http.Header
}

// parseJson parses a JSON body; ok=false on malformed JSON (caller answers 400).
func parseJson(raw []byte) (any, bool) {
	if len(raw) == 0 {
		return map[string]any{}, true
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	return v, true
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func getStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getBool(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func getInt(m map[string]any, key string, def int) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if i, err := strconv.Atoi(n); err == nil {
				return i
			}
		}
	}
	return def
}

func decodePath(s string) (string, error) { return url.PathUnescape(s) }

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
func jsonMarshal(v any) ([]byte, error)  { return json.Marshal(v) }

func notFound(w http.ResponseWriter)       { jsonReply(w, 404, map[string]any{"message": "Not Found"}, nil) }
func unauth(w http.ResponseWriter)         { jsonReply(w, 401, map[string]any{"message": "Requires authentication"}, nil) }
func badReq(w http.ResponseWriter, msg string) { jsonReply(w, 400, map[string]any{"message": msg}, nil) }
