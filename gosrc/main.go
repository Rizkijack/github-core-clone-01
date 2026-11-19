package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var gitPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git(/.*)?$`)

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func makeHandler(store *Store, expected string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pathname := r.URL.Path
		method := r.Method

		if method == "GET" && pathname == "/healthz" {
			jsonReply(w, 200, map[string]any{"ok": true}, nil)
			return
		}

		token := bearer(r)
		authed := token != "" && (expected == "" || token == expected)
		var rawBody []byte
		if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
			rawBody = readBody(r)
		}
		ctx := &Ctx{Method: method, Pathname: pathname, Query: r.URL.Query(), RawBody: rawBody, Authed: authed, Host: r.Host, Header: r.Header}

		if pathname == "/api/graphql" || pathname == "/api/graphql/" {
			store.mu.Lock()
			handleGraphQL(store, ctx, w)
			store.mu.Unlock()
			return
		}
		if pathname == "/mcp" || pathname == "/mcp/" {
			store.mu.Lock()
			handleMcp(store, ctx, w)
			store.mu.Unlock()
			return
		}
		if m := gitPathRe.FindStringSubmatch(pathname); m != nil {
			owner, _ := urlDecode(m[1])
			repoName, _ := urlDecode(m[2])
			store.mu.RLock()
			repo := store.getRepo(owner, repoName)
			store.mu.RUnlock()
			if repo == nil {
				notFound(w)
				return
			}
			gitPath := m[3]
			write := method == "POST" && strings.HasSuffix(gitPath, "/git-receive-pack")
			if write {
				store.mu.Lock()
			} else {
				store.mu.RLock()
			}
			handleGit(store, ctx, w, repo, gitPath)
			if write {
				store.mu.Unlock()
			} else {
				store.mu.RUnlock()
			}
			return
		}
		if pathname == "/api/v3" || strings.HasPrefix(pathname, "/api/v3/") {
			if method == "GET" {
				store.mu.RLock()
			} else {
				store.mu.Lock()
			}
			handleRest(store, ctx, w)
			if method == "GET" {
				store.mu.RUnlock()
			} else {
				store.mu.Unlock()
			}
			return
		}
		notFound(w)
	}
}

func urlDecode(s string) (string, error) {
	return decodePath(s)
}

func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	resp, err := http.Post(envURL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func waitDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var v struct {
				Done bool `json:"done"`
			}
			json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
			if v.Done {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func main() {
	store := newStore()
	expected := os.Getenv("MAKEFUN_SHOP_TOKEN")
	handler := makeHandler(store, expected)
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

	if envURL != "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, "listen:", err)
			os.Exit(1)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if err := register(envURL, port); err != nil {
			fmt.Fprintln(os.Stderr, "register:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "[forge] registered port %d\n", port)
		go func() {
			waitDone(envURL)
			os.Exit(0)
		}()
		http.Serve(ln, handler)
		return
	}

	// Standalone mode for local testing: bind PORT and serve without the env lifecycle.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "[forge] standalone listening on %s\n", ln.Addr().String())
	http.Serve(ln, handler)
}
