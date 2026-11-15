package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHostRoutingPreservesPathAndQuery(test *testing.T) {
	upstreamA := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/library/nginx/manifests/latest" {
			test.Errorf("upstream A path = %q", request.URL.Path)
		}
		if request.URL.RawQuery != "a=1" {
			test.Errorf("upstream A query = %q", request.URL.RawQuery)
		}
		writer.Write([]byte("A"))
	}))
	defer upstreamA.Close()
	upstreamB := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("B"))
	}))
	defer upstreamB.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"go.internal"}, Upstream: upstreamA.URL},
			{Hosts: []string{"registry.internal"}, Upstream: upstreamB.URL},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/v2/library/nginx/manifests/latest?a=1", nil)
	request.Host = "go.internal"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "A" {
		test.Fatalf("go.internal response = %d %q", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/v2/", nil)
	request.Host = "registry.internal"
	recorder = httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "B" {
		test.Fatalf("registry.internal response = %d %q", recorder.Code, recorder.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "unknown.internal"
	recorder = httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		test.Fatalf("unknown host status = %d, want 404", recorder.Code)
	}
}

func TestWildcardAndCatchAllHosts(test *testing.T) {
	exactUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("exact"))
	}))
	defer exactUpstream.Close()
	wildcardUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("wildcard"))
	}))
	defer wildcardUpstream.Close()
	catchAllUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("catch-all"))
	}))
	defer catchAllUpstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"exact.go.internal"}, Upstream: exactUpstream.URL},
			{Hosts: []string{"*.go.internal"}, Upstream: wildcardUpstream.URL},
			{Hosts: []string{"*"}, Upstream: catchAllUpstream.URL},
		},
	})
	cases := map[string]string{
		"exact.go.internal": "exact",
		"a.go.internal":     "wildcard",
		"other.internal":    "catch-all",
	}
	for host, expected := range cases {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Host = host
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)
		if recorder.Body.String() != expected {
			test.Fatalf("host %q body = %q, want %q", host, recorder.Body.String(), expected)
		}
	}
}

func TestPathCacheControlOverrides(test *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "upstream")
		writer.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{
				Hosts:        []string{"go.internal"},
				Upstream:     upstream.URL,
				CacheControl: stringPointer("site"),
				Paths: []PathRuleConfig{
					{Regex: `\.zip$`, CacheControl: stringPointer("zip")},
					{Regex: `/@v/`, CacheControl: stringPointer("meta")},
				},
			},
		},
	})
	cases := map[string]string{
		"/module/@v/v1.0.0.zip": "zip",
		"/module/@v/list":       "meta",
		"/other":                "site",
	}
	for path, expected := range cases {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Host = "go.internal"
		recorder := httptest.NewRecorder()
		proxy.ServeHTTP(recorder, request)
		if actual := recorder.Header().Get("Cache-Control"); actual != expected {
			test.Fatalf("path %q Cache-Control = %q, want %q", path, actual, expected)
		}
	}
}

func TestCacheControlCanBeCleared(test *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "upstream")
		writer.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"go.internal"}, Upstream: upstream.URL, CacheControl: stringPointer("")},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "go.internal"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if actual := recorder.Header().Get("Cache-Control"); actual != "" {
		test.Fatalf("Cache-Control = %q, want empty", actual)
	}
}

func TestFollowRedirectAndApplyOriginalPathCacheControl(test *testing.T) {
	finalUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/blob.zip" {
			test.Errorf("final path = %q", request.URL.Path)
		}
		writer.Header().Set("Cache-Control", "final")
		writer.Write([]byte("final"))
	}))
	defer finalUpstream.Close()
	redirectUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, finalUpstream.URL+"/blob.zip", http.StatusFound)
	}))
	defer redirectUpstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{
				Hosts:    []string{"go.internal"},
				Upstream: redirectUpstream.URL,
				Paths: []PathRuleConfig{
					{Regex: `\.zip$`, CacheControl: stringPointer("zip")},
				},
			},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/blob.zip", nil)
	request.Host = "go.internal"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "final" {
		test.Fatalf("redirect response = %d %q", recorder.Code, recorder.Body.String())
	}
	if actual := recorder.Header().Get("Cache-Control"); actual != "zip" {
		test.Fatalf("Cache-Control = %q, want zip", actual)
	}
}

func TestDisableRedirectPassesLocation(test *testing.T) {
	redirectUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "https://storage.example.internal/blob.zip", http.StatusFound)
	}))
	defer redirectUpstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"go.internal"}, Upstream: redirectUpstream.URL, FollowRedirects: boolPointer(false)},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/blob.zip", nil)
	request.Host = "go.internal"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound {
		test.Fatalf("status = %d, want 302", recorder.Code)
	}
	if actual := recorder.Header().Get("Location"); actual != "https://storage.example.internal/blob.zip" {
		test.Fatalf("Location = %q", actual)
	}
}

func TestMaxRedirects(test *testing.T) {
	loopUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, request.URL.Path, http.StatusFound)
	}))
	defer loopUpstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"go.internal"}, Upstream: loopUpstream.URL, MaxRedirects: intPointer(2)},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/loop", nil)
	request.Host = "go.internal"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		test.Fatalf("status = %d, want 502", recorder.Code)
	}
}

func TestPathTimeoutReturnsGatewayTimeout(test *testing.T) {
	slowUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			writer.Write([]byte("late"))
		case <-request.Context().Done():
		}
	}))
	defer slowUpstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{
				Hosts:    []string{"go.internal"},
				Upstream: slowUpstream.URL,
				Paths: []PathRuleConfig{
					{Prefix: "/slow", Timeout: durationPointer(20 * time.Millisecond)},
				},
			},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/slow/blob.zip", nil)
	request.Host = "go.internal"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusGatewayTimeout {
		test.Fatalf("status = %d, want 504", recorder.Code)
	}
}

func TestHopHeadersAreRemoved(test *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Remove") != "" {
			test.Errorf("X-Remove was forwarded")
		}
		if request.Header.Get("Proxy-Authorization") != "" {
			test.Errorf("Proxy-Authorization was forwarded")
		}
		writer.Header().Set("Connection", "X-Response-Remove")
		writer.Header().Set("X-Response-Remove", "bad")
		writer.Header().Set("X-Keep", "ok")
		writer.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"go.internal"}, Upstream: upstream.URL},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "go.internal"
	request.Header.Set("Connection", "X-Remove")
	request.Header.Set("X-Remove", "bad")
	request.Header.Set("Proxy-Authorization", "bad")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Header().Get("X-Response-Remove") != "" {
		test.Fatal("response hop header was forwarded")
	}
	if recorder.Header().Get("X-Keep") != "ok" {
		test.Fatal("non-hop response header was lost")
	}
}

func TestUpstreamBasePathIsPreserved(test *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/prefix/v2/foo" {
			test.Errorf("path = %q, want /prefix/v2/foo", request.URL.Path)
		}
		writer.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"go.internal"}, Upstream: upstream.URL + "/prefix"},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/v2/foo", nil)
	request.Host = "go.internal"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		test.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestHeadRequest(test *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "3")
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"go.internal"}, Upstream: upstream.URL},
		},
	})
	request := httptest.NewRequest(http.MethodHead, "/blob.zip", nil)
	request.Host = "go.internal"
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		test.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		test.Fatalf("HEAD body = %q, want empty", recorder.Body.String())
	}
}

func TestCrossHostRedirectDropsAuthorization(test *testing.T) {
	finalUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "" {
			test.Errorf("Authorization was forwarded to redirect target")
		}
		writer.Write([]byte("final"))
	}))
	defer finalUpstream.Close()
	redirectUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			test.Errorf("initial Authorization = %q", request.Header.Get("Authorization"))
		}
		location := strings.Replace(finalUpstream.URL, "127.0.0.1", "localhost", 1) + "/blob.zip"
		http.Redirect(writer, request, location, http.StatusFound)
	}))
	defer redirectUpstream.Close()
	proxy := mustTestProxy(test, &Config{
		Sites: []SiteConfig{
			{Hosts: []string{"go.internal"}, Upstream: redirectUpstream.URL},
		},
	})
	request := httptest.NewRequest(http.MethodGet, "/blob.zip", nil)
	request.Host = "go.internal"
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "final" {
		test.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func mustTestProxy(test *testing.T, cfg *Config) *Proxy {
	test.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxy, err := NewProxy(cfg, logger)
	if err != nil {
		test.Fatalf("NewProxy returned error: %v", err)
	}
	test.Cleanup(proxy.CloseIdleConnections)
	return proxy
}
