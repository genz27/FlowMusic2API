package service

import (
	"crypto/tls"
	"net/http"
	"net/url"
	"strings"

	"flowmusic2api/internal/config"
)

func NewHTTPClient(cfg config.Config, proxyURL string) *http.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.TLSInsecureSkipVerify,
		},
		ResponseHeaderTimeout: cfg.UpstreamTimeout,
	}
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	return &http.Client{
		Timeout:   cfg.UpstreamTimeout,
		Transport: transport,
	}
}
