package rerank

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// This documents the current provider contract, not an automatic URL repair.
func TestRerankerEndpointContract(t *testing.T) {
	withRerankSSRFWhitelist(t, "127.0.0.1")
	for _, tc := range []struct {
		name, provider, basePath, wantPath string
		wantError                          bool
	}{
		{"zhipu_root_is_not_completed", "zhipu", "/api/paas/v4", "/api/paas/v4", true},
		{"zhipu_complete_endpoint", "zhipu", "/api/paas/v4/rerank", "/api/paas/v4/rerank", false},
		{"generic_appends_rerank", "generic", "/v1", "/v1/rerank", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths <- r.URL.Path
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("expected authenticated POST")
				}
				var request struct {
					Model     string   `json:"model"`
					Query     string   `json:"query"`
					Documents []string `json:"documents"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if request.Model != "test-rerank" || request.Query != "query" || len(request.Documents) != 2 {
					t.Error("factory did not preserve model/query/documents")
				}
				if !strings.HasSuffix(r.URL.Path, "/rerank") {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"results":[{"index":1,"relevance_score":0.9}]}`)
			}))
			defer server.Close()
			model := &types.Model{Name: "test-rerank", Source: types.ModelSourceRemote,
				Parameters: types.ModelParameters{Provider: tc.provider, BaseURL: server.URL + tc.basePath, APIKey: "test-key"}}
			r, err := NewReranker(ConfigFromModel(model, "", ""))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			results, err := r.Rerank(ctx, "query", []string{"irrelevant", "relevant"})
			select {
			case path := <-paths:
				if path != tc.wantPath {
					t.Errorf("request path = %q, want %q", path, tc.wantPath)
				}
			default:
				t.Fatal("request never reached server")
			}
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "404") {
					t.Fatalf("expected HTTP 404, got %v", err)
				}
			} else if err != nil || len(results) != 1 || results[0].Index != 1 || results[0].RelevanceScore != 0.9 {
				t.Fatalf("unexpected ranking: results=%v err=%v", results, err)
			}
		})
	}
}

func TestRerankerDeadline(t *testing.T) {
	withRerankSSRFWhitelist(t, "127.0.0.1")
	for _, provider := range []string{"zhipu", "generic"} {
		for _, phase := range []string{"headers", "body"} {
			t.Run(provider+"/"+phase, func(t *testing.T) {
				entered := make(chan struct{}, 1)
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if phase == "body" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"results":`)
						w.(http.Flusher).Flush()
					}
					entered <- struct{}{}
					select {
					case <-r.Context().Done():
					case <-release:
					}
				}))
				defer server.Close()
				defer close(release)
				r, err := NewReranker(&RerankerConfig{Provider: provider, BaseURL: server.URL, ModelName: "test-rerank"})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
				defer cancel()
				start := time.Now()
				_, err = r.Rerank(ctx, "query", []string{"doc"})
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expected deadline error while waiting for %s, got %v", phase, err)
				}
				if elapsed := time.Since(start); elapsed > 3*time.Second {
					t.Errorf("deadline returned too late: %s", elapsed)
				}
				select {
				case <-entered:
				default:
					t.Fatal("deadline expired before server received request; did not exercise target phase")
				}
			})
		}
	}
}

// TestRerankerConnectivityLive makes real, billable calls only when opted in.
// See docs/reranker-connectivity.md for commands, transport controls and limits.
// No parallel subtests: the proxy control swaps a test-process-only transport.
func TestRerankerConnectivityLive(t *testing.T) {
	if os.Getenv("WEKNORA_RERANK_LIVE") != "1" {
		t.Skip("set WEKNORA_RERANK_LIVE=1 to call the configured service")
	}
	// Load existing security policy and YAML credential substitutions without
	// activating unrelated application settings. Proxy variables come from the shell.
	var fileEnv map[string]string
	if envPath := os.Getenv("WEKNORA_RERANK_ENV_FILE"); envPath != "" {
		values, err := godotenv.Read(envPath)
		if err != nil {
			t.Fatal("cannot read diagnostic environment file")
		}
		fileEnv = values
		for _, key := range []string{"SSRF_WHITELIST", "SSRF_WHITELIST_EXTRA"} {
			if _, exists := os.LookupEnv(key); !exists {
				if value, found := values[key]; found {
					t.Setenv(key, value)
				}
			}
		}
		secutils.ResetSSRFWhitelistForTest()
		t.Cleanup(secutils.ResetSSRFWhitelistForTest)
	}
	configPath := os.Getenv("WEKNORA_RERANK_CONFIG")
	if configPath == "" {
		configPath = filepath.Join("..", "..", "..", "config", "builtin_models.yaml")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("cannot read reranker YAML configuration")
	}
	// Match the application's ${NAME} interpolation; unset values remain visible
	// to validation rather than silently replacing a credential with empty text.
	expanded := regexp.MustCompile(`\$\{([^}]+)\}`).ReplaceAllStringFunc(string(data), func(match string) string {
		if value := os.Getenv(match[2 : len(match)-1]); value != "" {
			return value
		}
		if value := fileEnv[match[2:len(match)-1]]; value != "" {
			return value
		}
		return match
	})
	var file struct {
		Models []types.BuiltinModelEntry `yaml:"builtin_models"`
	}
	if err := yaml.Unmarshal([]byte(expanded), &file); err != nil {
		t.Fatal("invalid reranker YAML configuration (details suppressed to protect credentials)")
	}
	id := os.Getenv("WEKNORA_RERANK_MODEL_ID")
	if id == "" {
		id = "builtin-rerank"
	}
	var config *RerankerConfig
	for _, entry := range file.Models {
		if entry.ID == id && entry.Type == types.ModelTypeRerank {
			config = ConfigFromModel(&types.Model{ID: entry.ID, Name: entry.Name, Source: entry.Source, Parameters: entry.Parameters}, "", "")
			break
		}
	}
	if config == nil {
		t.Fatal("selected rerank model not found in YAML")
	}
	if config.APIKey == "" || strings.Contains(config.APIKey, "${") {
		t.Fatal("selected model has missing or unresolved API key")
	}
	if config.Provider != "zhipu" && config.Provider != "generic" && config.Provider != "gpustack" && config.Provider != "openai" {
		t.Fatal("diagnostic supports only zhipu and OpenAI-compatible rerank providers")
	}
	u, err := url.Parse(config.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		t.Fatal("diagnostic requires an HTTP(S) endpoint without URL credentials, query or fragment")
	}
	mode := os.Getenv("WEKNORA_RERANK_TRANSPORT")
	if mode == "" {
		mode = "production"
	}
	switch mode {
	case "production":
	case "environment_proxy":
		original := sharedRerankHTTPTransport
		control := secutils.NewSSRFSafeTransportWithEnvironmentProxy(secutils.DefaultSSRFSafeHTTPClientConfig())
		sharedRerankHTTPTransport = control
		t.Cleanup(func() {
			sharedRerankHTTPTransport = original
			control.CloseIdleConnections()
		})
	default:
		t.Fatal("WEKNORA_RERANK_TRANSPORT must be production or environment_proxy")
	}
	proxyAddress := "none"
	if sharedRerankHTTPTransport.Proxy != nil {
		proxy, proxyErr := sharedRerankHTTPTransport.Proxy(&http.Request{URL: u})
		if proxyErr != nil {
			t.Fatal("invalid environment proxy configuration")
		}
		if proxy != nil {
			proxyAddress = proxy.Scheme + "://" + proxy.Host
		}
	}
	t.Logf("source=YAML model_id=%s model=%s provider=%s transport=%s selected_proxy=%s deadline=30s (DB overrides are not loaded)", id, config.ModelName, config.Provider, mode, proxyAddress)
	cases := []struct{ name, baseURL string }{{"configured", config.BaseURL}}
	if config.Provider == "zhipu" && !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/rerank") {
		cases = append(cases, struct{ name, baseURL string }{"complete_endpoint", strings.TrimRight(config.BaseURL, "/") + "/rerank"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *config
			cfg.BaseURL = tc.baseURL
			start := time.Now()
			r, err := NewReranker(&cfg)
			if err != nil {
				t.Fatalf("factory failed after %s: %s", time.Since(start), redactProbeError(err, &cfg))
			}
			t.Logf("base_url=%s factory_elapsed=%s", cfg.BaseURL, time.Since(start))
			start = time.Now()
			var mu sync.Mutex
			var events []string
			record := func(event string) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, time.Since(start).Round(time.Millisecond).String()+" "+event)
			}
			trace := &httptrace.ClientTrace{
				DNSStart: func(httptrace.DNSStartInfo) { record("dns_start") },
				DNSDone: func(info httptrace.DNSDoneInfo) {
					if info.Err == nil {
						record("dns_done")
					} else {
						record("dns_failed")
					}
				},
				ConnectStart: func(_, addr string) { record("connect_start " + addr) },
				ConnectDone: func(_, addr string, err error) {
					if err == nil {
						record("connect_ok " + addr)
					} else {
						record("connect_failed " + addr)
					}
				},
				TLSHandshakeStart: func() { record("tls_start") },
				TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
					if err == nil {
						record("tls_ok")
					} else {
						record("tls_failed")
					}
				},
				GotConn: func(info httptrace.GotConnInfo) {
					if info.Reused {
						record("connection_reused")
					} else {
						record("connection_ready")
					}
				},
				WroteRequest: func(info httptrace.WroteRequestInfo) {
					if info.Err == nil {
						record("request_written")
					} else {
						record("request_write_failed")
					}
				},
				GotFirstResponseByte: func() { record("first_response_byte") },
			}
			ctx, cancel := context.WithTimeout(httptrace.WithClientTrace(t.Context(), trace), 30*time.Second)
			defer cancel()
			documents := []string{"索道升级包含知识库管理模块。", "今天天气晴朗。"}
			results, callErr := r.Rerank(ctx, "索道升级包含什么模块？", documents)
			mu.Lock()
			t.Logf("elapsed=%s phases=%s", time.Since(start).Round(time.Millisecond), strings.Join(events, "; "))
			mu.Unlock()
			if callErr != nil {
				t.Fatalf("rerank failed: %s", redactProbeError(callErr, &cfg))
			}
			if len(results) == 0 || len(results) > len(documents) {
				t.Fatalf("invalid ranking count: %d", len(results))
			}
			seen := make(map[int]bool)
			for _, result := range results {
				if result.Index < 0 || result.Index >= len(documents) || seen[result.Index] || math.IsNaN(result.RelevanceScore) || math.IsInf(result.RelevanceScore, 0) {
					t.Fatal("invalid ranking index or score")
				}
				seen[result.Index] = true
				t.Logf("result index=%d score=%.6f", result.Index, result.RelevanceScore)
			}
		})
	}
}

func redactProbeError(err error, cfg *RerankerConfig) string {
	message := err.Error()
	for _, secret := range append([]string{cfg.APIKey, cfg.AppSecret}, headerValues(cfg.CustomHeaders)...) {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	if len(message) > 1000 {
		message = message[:1000] + " [truncated]"
	}
	return message
}

func headerValues(headers map[string]string) []string {
	values := make([]string, 0, len(headers))
	for _, value := range headers {
		values = append(values, value)
	}
	return values
}
