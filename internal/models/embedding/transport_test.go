package embedding

import (
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	secutils "github.com/Tencent/WeKnora/internal/utils"
)

func TestEmbeddingTransportEnvironmentProxy(t *testing.T) {
	const helper = "WEKNORA_EMBEDDING_PROXY_TEST"
	if os.Getenv(helper) == "1" {
		if sharedEmbeddingHTTPTransport.Proxy == nil {
			t.Fatal("embedding transport must honor environment proxy settings")
		}
		for _, scheme := range []string{"http", "https"} {
			req, err := http.NewRequest(http.MethodPost, scheme+"://embedding.example.test/v1/embeddings", nil)
			if err != nil {
				t.Fatal(err)
			}
			proxy, err := sharedEmbeddingHTTPTransport.Proxy(req)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if proxy != nil {
				got = proxy.String()
			}
			if want := os.Getenv("WEKNORA_EMBEDDING_PROXY_EXPECTED"); got != want {
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
				expected = "http://127.0.0.1:17897"
				t.Setenv("http_proxy", expected)
				t.Setenv("https_proxy", expected)
			}
			if mode == "no_proxy" {
				t.Setenv("NO_PROXY", "embedding.example.test")
				expected = ""
			}
			t.Setenv(helper, "1")
			t.Setenv("WEKNORA_EMBEDDING_PROXY_EXPECTED", expected)
			cmd := exec.Command(os.Args[0], "-test.run=^TestEmbeddingTransportEnvironmentProxy$")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("proxy selection check failed: %v\n%s", err, output)
			}
		})
	}
}

func TestNewEmbeddingHTTPClient_ReusesTransport(t *testing.T) {
	firstTimeout := 15 * time.Second
	secondTimeout := 45 * time.Second
	first := newEmbeddingHTTPClient(firstTimeout)
	second := newEmbeddingHTTPClient(secondTimeout)

	if first == second {
		t.Fatal("expected distinct HTTP clients")
	}
	firstGuard, ok := first.Transport.(*secutils.SSRFValidatingRoundTripper)
	if !ok {
		t.Fatalf("expected SSRF-validating transport, got %T", first.Transport)
	}
	secondGuard, ok := second.Transport.(*secutils.SSRFValidatingRoundTripper)
	if !ok {
		t.Fatalf("expected SSRF-validating transport, got %T", second.Transport)
	}
	if firstGuard.Base != secondGuard.Base {
		t.Fatal("expected embedding HTTP clients to share a base transport")
	}
	if firstGuard.Base != http.RoundTripper(sharedEmbeddingHTTPTransport) {
		t.Fatal("expected embedding HTTP client to use the shared transport")
	}
	if first.Timeout != firstTimeout {
		t.Fatalf("unexpected first client timeout: got %v, want %v", first.Timeout, firstTimeout)
	}
	if second.Timeout != secondTimeout {
		t.Fatalf("unexpected second client timeout: got %v, want %v", second.Timeout, secondTimeout)
	}
}

func TestValidateEmbeddingBaseURL_RejectsLoopback(t *testing.T) {
	err := validateEmbeddingBaseURL("http://169.254.169.254/latest/meta-data")
	if err == nil {
		t.Fatal("expected SSRF error for link-local metadata URL")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateEmbeddingBaseURL_AllowsEmpty(t *testing.T) {
	if err := validateEmbeddingBaseURL(""); err != nil {
		t.Fatalf("empty base URL should be allowed: %v", err)
	}
}

func TestNewOpenAIEmbedder_RejectsPrivateBaseURL(t *testing.T) {
	_, err := NewOpenAIEmbedder(
		"test-key",
		"http://169.254.169.254/latest/meta-data",
		"text-embedding-3-small",
		511,
		256,
		"model-id",
		nil,
	)
	if err == nil {
		t.Fatal("expected SSRF rejection for link-local metadata URL")
	}
}
