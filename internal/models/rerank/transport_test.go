package rerank

import (
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	secutils "github.com/Tencent/WeKnora/internal/utils"
)

func TestRerankTransportEnvironmentProxy(t *testing.T) {
	const helper = "WEKNORA_RERANK_PROXY_TEST"
	if os.Getenv(helper) == "1" {
		if sharedRerankHTTPTransport.Proxy == nil {
			t.Fatal("rerank transport must honor environment proxy settings")
		}
		for _, scheme := range []string{"http", "https"} {
			req, err := http.NewRequest(http.MethodPost, scheme+"://rerank.example.test/v1/rerank", nil)
			if err != nil {
				t.Fatal(err)
			}
			proxy, err := sharedRerankHTTPTransport.Proxy(req)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if proxy != nil {
				got = proxy.String()
			}
			if want := os.Getenv("WEKNORA_RERANK_PROXY_EXPECTED"); got != want {
				t.Fatalf("%s proxy = %q, want %q", scheme, got, want)
			}
		}
		return
	}

	// net/http caches proxy settings, so each environment needs a fresh process.
	for _, mode := range []string{"proxy", "no_proxy", "direct"} {
		t.Run(mode, func(t *testing.T) {
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy", "REQUEST_METHOD"} {
				t.Setenv(key, "")
			}
			expected := ""
			if mode != "direct" {
				expected = "http://127.0.0.1:17898"
				t.Setenv("http_proxy", expected)
				t.Setenv("https_proxy", expected)
			}
			if mode == "no_proxy" {
				t.Setenv("NO_PROXY", "rerank.example.test")
				expected = ""
			}
			t.Setenv(helper, "1")
			t.Setenv("WEKNORA_RERANK_PROXY_EXPECTED", expected)
			cmd := exec.Command(os.Args[0], "-test.run=^TestRerankTransportEnvironmentProxy$")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("proxy selection check failed: %v\n%s", err, output)
			}
		})
	}
}

func TestNewRerankHTTPClientKeepsSSRFSafeguards(t *testing.T) {
	withRerankSSRFWhitelist(t, "")

	client := newRerankHTTPClient(15 * time.Second)
	guard, ok := client.Transport.(*secutils.SSRFValidatingRoundTripper)
	if !ok {
		t.Fatalf("expected SSRF-validating transport, got %T", client.Transport)
	}
	if guard.Base != sharedRerankHTTPTransport {
		t.Fatal("expected rerank HTTP client to use the shared transport")
	}
	if sharedRerankHTTPTransport.Proxy == nil {
		t.Fatal("expected rerank transport to select an environment proxy")
	}
	if sharedRerankHTTPTransport.DialContext == nil {
		t.Fatal("expected rerank transport to retain a dial-time SSRF guard")
	}
	if got, want := reflect.ValueOf(sharedRerankHTTPTransport.DialContext).Pointer(), reflect.ValueOf(secutils.SSRFSafeDialContext).Pointer(); got != want {
		t.Fatal("rerank transport replaced the SSRF-safe dialer")
	}
	if client.CheckRedirect == nil {
		t.Fatal("expected rerank HTTP client to retain redirect validation")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil || !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("internal URL error = %v, want SSRF rejection", err)
	}
}
