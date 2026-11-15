package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

type Proxy struct {
	logger        *slog.Logger
	transport     *http.Transport
	exactSites    map[string]*site
	wildcardSites []wildcardSite
	catchAllSite  *site
}

type wildcardSite struct {
	pattern string
	site    *site
}

type site struct {
	upstream     *url.URL
	client       *http.Client
	timeout      time.Duration
	cacheControl *string
	rules        []pathRule
}

type pathRule struct {
	prefix       string
	regex        *regexp.Regexp
	timeout      *time.Duration
	cacheControl *string
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (body *cancelOnCloseBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(body.cancel)
	return err
}

func NewProxy(cfg *Config, logger *slog.Logger) (*Proxy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	proxy := &Proxy{
		logger:     logger,
		transport:  transport,
		exactSites: make(map[string]*site),
	}
	for _, siteCfg := range cfg.Sites {
		upstream, err := parseUpstream(siteCfg.Upstream)
		if err != nil {
			return nil, err
		}
		timeout := durationValue(siteCfg.Timeout, 30*time.Second)
		followRedirects := boolValue(siteCfg.FollowRedirects, true)
		maxRedirects := intValue(siteCfg.MaxRedirects, 10)
		compiledSite := &site{
			upstream: upstream,
			client: &http.Client{
				Transport:     transport,
				CheckRedirect: redirectPolicy(followRedirects, maxRedirects),
			},
			timeout:      timeout,
			cacheControl: siteCfg.CacheControl,
			rules:        make([]pathRule, 0, len(siteCfg.Paths)),
		}
		for _, ruleCfg := range siteCfg.Paths {
			compiledRule := pathRule{
				prefix:       ruleCfg.Prefix,
				cacheControl: ruleCfg.CacheControl,
			}
			if ruleCfg.Regex != "" {
				compiledRule.regex = regexp.MustCompile(ruleCfg.Regex)
			}
			if ruleCfg.Timeout != nil {
				ruleTimeout := ruleCfg.Timeout.Duration
				compiledRule.timeout = &ruleTimeout
			}
			compiledSite.rules = append(compiledSite.rules, compiledRule)
		}
		for _, rawHost := range siteCfg.Hosts {
			host := normalizeHost(rawHost)
			switch {
			case host == "*":
				proxy.catchAllSite = compiledSite
			case strings.HasPrefix(host, "*."):
				proxy.wildcardSites = append(proxy.wildcardSites, wildcardSite{pattern: host, site: compiledSite})
			default:
				proxy.exactSites[host] = compiledSite
			}
		}
	}
	return proxy, nil
}

func (proxy *Proxy) CloseIdleConnections() {
	proxy.transport.CloseIdleConnections()
}

func (proxy *Proxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	host := normalizeHost(request.Host)
	if host == "" {
		http.Error(writer, "missing host", http.StatusBadRequest)
		proxy.logRequest(request, host, http.StatusBadRequest, started, "", nil)
		return
	}
	matchedSite := proxy.lookupSite(host)
	if matchedSite == nil {
		http.Error(writer, "unknown host", http.StatusNotFound)
		proxy.logRequest(request, host, http.StatusNotFound, started, "", nil)
		return
	}
	matchedRule := matchPathRule(matchedSite.rules, request.URL.Path)
	timeout := matchedSite.timeout
	cacheControl := matchedSite.cacheControl
	if matchedRule != nil {
		if matchedRule.timeout != nil {
			timeout = *matchedRule.timeout
		}
		if matchedRule.cacheControl != nil {
			cacheControl = matchedRule.cacheControl
		}
	}
	target, err := joinUpstreamURL(matchedSite.upstream, request.URL)
	if err != nil {
		http.Error(writer, "invalid upstream URL", http.StatusBadGateway)
		proxy.logRequest(request, host, http.StatusBadGateway, started, matchedSite.upstream.Host, err)
		return
	}
	requestContext, cancel := context.WithCancel(request.Context())
	handedOff := false
	defer func() {
		if !handedOff {
			cancel()
		}
	}()
	outbound := request.Clone(requestContext)
	outbound.URL = target
	outbound.RequestURI = ""
	outbound.Host = ""
	outbound.Header = request.Header.Clone()
	removeHopHeaders(outbound.Header)
	outbound.Header.Del("Host")
	var timedOut atomic.Bool
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			timedOut.Store(true)
			cancel()
		})
		defer timer.Stop()
	}
	response, err := matchedSite.client.Do(outbound)
	if timer != nil {
		timer.Stop()
	}
	if err != nil {
		status := http.StatusBadGateway
		if timedOut.Load() {
			status = http.StatusGatewayTimeout
		}
		http.Error(writer, http.StatusText(status), status)
		proxy.logRequest(request, host, status, started, target.Host, err)
		return
	}
	if timedOut.Load() {
		response.Body.Close()
		http.Error(writer, http.StatusText(http.StatusGatewayTimeout), http.StatusGatewayTimeout)
		proxy.logRequest(request, host, http.StatusGatewayTimeout, started, target.Host, fmt.Errorf("upstream response timed out"))
		return
	}
	if response.Body == nil {
		response.Body = http.NoBody
	}
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, cancel: cancel}
	handedOff = true
	defer response.Body.Close()
	responseHeader := response.Header.Clone()
	removeHopHeaders(responseHeader)
	applyCacheControl(responseHeader, cacheControl)
	copyHeader(writer.Header(), responseHeader)
	writer.WriteHeader(response.StatusCode)
	_, copyErr := io.Copy(writer, response.Body)
	proxy.logRequest(request, host, response.StatusCode, started, target.Host, copyErr)
}

func (proxy *Proxy) lookupSite(host string) *site {
	if matchedSite, exists := proxy.exactSites[host]; exists {
		return matchedSite
	}
	for _, candidate := range proxy.wildcardSites {
		suffix := candidate.pattern[1:]
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return candidate.site
		}
	}
	return proxy.catchAllSite
}

func (proxy *Proxy) logRequest(request *http.Request, host string, status int, started time.Time, upstreamHost string, requestErr error) {
	attributes := []any{
		"host", host,
		"method", request.Method,
		"path", request.URL.Path,
		"status", status,
		"duration_ms", time.Since(started).Milliseconds(),
		"upstream", upstreamHost,
	}
	if requestErr != nil {
		attributes = append(attributes, "error", requestErr.Error())
		proxy.logger.Error("proxy request", attributes...)
		return
	}
	proxy.logger.Info("proxy request", attributes...)
}

func redirectPolicy(followRedirects bool, maxRedirects int) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if !followRedirects || maxRedirects == 0 {
			return http.ErrUseLastResponse
		}
		if len(via) > maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
			return fmt.Errorf("unsupported redirect scheme %q", request.URL.Scheme)
		}
		return nil
	}
}

func matchPathRule(rules []pathRule, path string) *pathRule {
	for index := range rules {
		rule := &rules[index]
		if rule.prefix != "" && strings.HasPrefix(path, rule.prefix) {
			return rule
		}
		if rule.regex != nil && rule.regex.MatchString(path) {
			return rule
		}
	}
	return nil
}

func joinUpstreamURL(base *url.URL, incoming *url.URL) (*url.URL, error) {
	baseURL := strings.TrimSuffix(base.String(), "/")
	requestPath := incoming.EscapedPath()
	if requestPath == "" {
		requestPath = "/"
	}
	if !strings.HasPrefix(requestPath, "/") {
		requestPath = "/" + requestPath
	}
	joined := baseURL + requestPath
	if incoming.RawQuery != "" || incoming.ForceQuery {
		joined += "?" + incoming.RawQuery
	}
	target, err := url.Parse(joined)
	if err != nil {
		return nil, err
	}
	return target, nil
}

func removeHopHeaders(header http.Header) {
	for _, connectionValue := range header.Values("Connection") {
		for _, headerName := range strings.Split(connectionValue, ",") {
			header.Del(strings.TrimSpace(headerName))
		}
	}
	for _, headerName := range hopHeaders {
		header.Del(headerName)
	}
}

func applyCacheControl(header http.Header, override *string) {
	if override == nil {
		return
	}
	if *override == "" {
		header.Del("Cache-Control")
		return
	}
	header.Set("Cache-Control", *override)
}

func copyHeader(destination http.Header, source http.Header) {
	for headerName, values := range source {
		for _, value := range values {
			destination.Add(headerName, value)
		}
	}
}

func durationValue(value *Duration, fallback time.Duration) time.Duration {
	if value == nil {
		return fallback
	}
	return value.Duration
}

func boolValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func intValue(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}
