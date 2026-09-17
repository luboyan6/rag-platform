package utils

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	ssrfProxyTestHelperEnv = "WEKNORA_SSRF_PROXY_TEST_HELPER"
	ssrfProxyTestModeEnv   = "WEKNORA_SSRF_PROXY_TEST_MODE"
	ssrfProxyTestTargetEnv = "WEKNORA_SSRF_PROXY_TEST_TARGET"
	ssrfProxyTestDialEnv   = "WEKNORA_SSRF_PROXY_TEST_DIAL_ADDR"
	ssrfProxyTestBodyEnv   = "WEKNORA_SSRF_PROXY_TEST_BODY"
)

// TestSSRFSafeTransport_EnvironmentProxy runs the actual HTTP transport in a
// child process. net/http caches ProxyFromEnvironment's parsed environment per
// process, so a subprocess is the only deterministic way to cover multiple
// proxy/NO_PROXY environments in one test suite.
func TestSSRFSafeTransport_EnvironmentProxy(t *testing.T) {
	if os.Getenv(ssrfProxyTestHelperEnv) == "1" {
		runSSRFSafeTransportProxyHelper(t)
		return
	}

	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "target")
	}))
	defer httpTarget.Close()

	httpsTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "target")
	}))
	defer httpsTarget.Close()

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.Method == http.MethodConnect {
			tunnelTestProxy(w, httpsTarget.Listener.Addr().String())
			return
		}
		_, _ = io.WriteString(w, "proxy")
	}))
	defer proxy.Close()

	cases := []struct {
		name       string
		mode       string
		target     string
		dialAddr   string
		httpProxy  string
		httpsProxy string
		noProxy    string
		whitelist  string
		wantBody   string
		wantHits   int32
	}{
		{
			name:      "HTTP_PROXY routes HTTP requests",
			mode:      "http",
			target:    "http://example.com/",
			dialAddr:  httpTarget.Listener.Addr().String(),
			httpProxy: proxy.URL,
			whitelist: "example.com,127.0.0.1",
			wantBody:  "proxy",
			wantHits:  1,
		},
		{
			name:       "HTTPS_PROXY routes HTTPS requests with CONNECT",
			mode:       "https",
			target:     "https://example.com/",
			httpsProxy: proxy.URL,
			whitelist:  "example.com,127.0.0.1",
			wantBody:   "target",
			wantHits:   1,
		},
		{
			name:      "NO_PROXY bypasses configured proxy",
			mode:      "http",
			target:    "http://example.com/",
			dialAddr:  httpTarget.Listener.Addr().String(),
			httpProxy: proxy.URL,
			noProxy:   "example.com",
			whitelist: "example.com,127.0.0.1",
			wantBody:  "target",
			wantHits:  0,
		},
		{
			name:      "no proxy keeps direct behavior",
			mode:      "http",
			target:    "http://example.com/",
			dialAddr:  httpTarget.Listener.Addr().String(),
			whitelist: "example.com,127.0.0.1",
			wantBody:  "target",
			wantHits:  0,
		},
		{
			name:      "proxy does not bypass SSRF target validation",
			mode:      "blocked",
			target:    httpTarget.URL,
			httpProxy: proxy.URL,
			wantHits:  0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxyHits.Store(0)
			runSSRFSafeTransportProxyChild(t, tc)
			if got := proxyHits.Load(); got != tc.wantHits {
				t.Fatalf("proxy hits = %d, want %d", got, tc.wantHits)
			}
		})
	}
}

func TestIsSystemProxyMatchesConfiguredProxyAddress(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://localhost:7897")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("https_proxy", "")

	if !IsSystemProxy("localhost:7897") {
		t.Fatal("expected configured HTTP proxy address to be recognized")
	}
	if IsSystemProxy("localhost:7898") {
		t.Fatal("did not expect a different address to be recognized as system proxy")
	}
}

func TestSSRFSafeTransport_EnvironmentProxyIsOptIn(t *testing.T) {
	config := DefaultSSRFSafeHTTPClientConfig()

	if transport := NewSSRFSafeTransport(config); transport.Proxy != nil {
		t.Fatal("expected the shared default transport to leave proxy selection disabled")
	}
	if transport := NewSSRFSafeTransportWithEnvironmentProxy(config); transport.Proxy == nil {
		t.Fatal("expected the opt-in transport to use environment proxy selection")
	}
}

func runSSRFSafeTransportProxyChild(t *testing.T, tc struct {
	name       string
	mode       string
	target     string
	dialAddr   string
	httpProxy  string
	httpsProxy string
	noProxy    string
	whitelist  string
	wantBody   string
	wantHits   int32
}) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run", "^TestSSRFSafeTransport_EnvironmentProxy$")
	cmd.Env = proxyTestEnvironment(map[string]string{
		ssrfProxyTestHelperEnv: "1",
		ssrfProxyTestModeEnv:   tc.mode,
		ssrfProxyTestTargetEnv: tc.target,
		ssrfProxyTestDialEnv:   tc.dialAddr,
		ssrfProxyTestBodyEnv:   tc.wantBody,
		"HTTP_PROXY":           tc.httpProxy,
		"HTTPS_PROXY":          tc.httpsProxy,
		"ALL_PROXY":            "",
		"NO_PROXY":             tc.noProxy,
		"SSRF_WHITELIST":       tc.whitelist,
		"SSRF_WHITELIST_EXTRA": "",
		"http_proxy":           tc.httpProxy,
		"https_proxy":          tc.httpsProxy,
		"all_proxy":            "",
		"no_proxy":             tc.noProxy,
	})

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("proxy helper failed: %v\n%s", err, output.String())
	}
}

func runSSRFSafeTransportProxyHelper(t *testing.T) {
	t.Helper()

	mode := os.Getenv(ssrfProxyTestModeEnv)
	client := NewSSRFSafeHTTPClientWithEnvironmentProxy(SSRFSafeHTTPClientConfig{
		Timeout:      5 * time.Second,
		MaxRedirects: 1,
	})
	guard, ok := client.Transport.(*SSRFValidatingRoundTripper)
	if !ok {
		t.Fatalf("expected SSRF-validating transport, got %T", client.Transport)
	}
	transport, ok := guard.Base.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", guard.Base)
	}
	if dialAddr := os.Getenv(ssrfProxyTestDialEnv); dialAddr != "" {
		originalDial := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == "example.com:80" || addr == "example.com:443" {
				addr = dialAddr
			}
			return originalDial(ctx, network, addr)
		}
	}
	if mode == "https" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only TLS server
	}

	resp, err := client.Get(os.Getenv(ssrfProxyTestTargetEnv))
	if mode == "blocked" {
		if err == nil {
			resp.Body.Close()
			t.Fatal("expected SSRF validation to reject the target")
		}
		if !strings.Contains(err.Error(), "SSRF") {
			t.Fatalf("expected SSRF validation error, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("GET through configured transport: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got, want := string(body), os.Getenv(ssrfProxyTestBodyEnv); got != want {
		t.Fatalf("response body = %q, want %q", got, want)
	}
}

func proxyTestEnvironment(overrides map[string]string) []string {
	keys := make(map[string]struct{}, len(overrides))
	for key := range overrides {
		keys[strings.ToUpper(key)] = struct{}{}
	}

	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := keys[strings.ToUpper(name)]; ok {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func tunnelTestProxy(w http.ResponseWriter, targetAddr string) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy does not support hijacking", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	upstream, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()

	if _, err := fmt.Fprint(clientConn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go copyProxyBytes(done, upstream, clientConn)
	go copyProxyBytes(done, clientConn, upstream)
	<-done
}

func copyProxyBytes(done chan<- struct{}, dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
	done <- struct{}{}
}
