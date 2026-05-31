package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
	"flowmusic2api/internal/store"
)

func TestCacheURLLocalStorage(t *testing.T) {
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("fake audio bytes"))
	}))
	t.Cleanup(media.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:                dir,
		CacheDir:               filepath.Join(dir, "tmp"),
		DatabaseDriver:         "sqlite",
		DatabaseURL:            filepath.Join(dir, "flowmusic2api.db"),
		UpstreamTimeout:        time.Second,
		StoragePresignDuration: time.Hour,
	}
	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := db.UpdateCacheConfig(ctx, domain.CacheConfig{
		Enabled:     true,
		StorageMode: "local",
		BaseURL:     "https://cdn.example.test",
	}); err != nil {
		t.Fatalf("UpdateCacheConfig() error = %v", err)
	}

	cache := NewCache(cfg, db, media.Client())
	ref, err := cache.CacheURL(ctx, media.URL+"/song.mp3")
	if err != nil {
		t.Fatalf("CacheURL() error = %v", err)
	}
	if ref.OriginalURL == "" || !strings.HasPrefix(ref.URL, "https://cdn.example.test/tmp/") {
		t.Fatalf("unexpected media ref: %+v", ref)
	}
	if ref.ContentType != "audio/mpeg" || ref.Size != int64(len("fake audio bytes")) {
		t.Fatalf("unexpected metadata: %+v", ref)
	}

	entries, err := os.ReadDir(cfg.CacheDir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("cache file count = %d, want 1", len(entries))
	}
}

func TestExtensionFromURLRecognizesAVIContentTypes(t *testing.T) {
	for _, contentType := range []string{"video/x-msvideo", "video/avi", "video/msvideo"} {
		if got := extensionFromURL("https://cdn.example.test/download", contentType); got != ".avi" {
			t.Fatalf("extensionFromURL(%q) = %q, want .avi", contentType, got)
		}
	}
}

func TestCacheURLS3CompatibleStorage(t *testing.T) {
	var putPath string
	var putBody string
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if r.Method != http.MethodPut {
			t.Fatalf("unexpected S3 method: %s %s", r.Method, r.URL.String())
		}
		putPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		putBody = string(body)
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s3.Close)

	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("fake s3 audio bytes"))
	}))
	t.Cleanup(media.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:                dir,
		CacheDir:               filepath.Join(dir, "tmp"),
		DatabaseDriver:         "sqlite",
		DatabaseURL:            filepath.Join(dir, "flowmusic2api.db"),
		UpstreamTimeout:        time.Second,
		StoragePresignDuration: time.Hour,
	}
	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := db.UpdateCacheConfig(ctx, domain.CacheConfig{
		Enabled:          true,
		StorageMode:      "s3",
		S3Endpoint:       s3.URL,
		S3Bucket:         "flowmusic-cache",
		S3UseSSL:         false,
		S3ForcePathStyle: true,
		S3Prefix:         "flow-assets",
		S3PublicBaseURL:  "https://cdn.example.test/cache",
	}); err != nil {
		t.Fatalf("UpdateCacheConfig() error = %v", err)
	}

	cache := NewCache(cfg, db, media.Client())
	ref, err := cache.CacheURL(ctx, media.URL+"/song.mp3")
	if err != nil {
		t.Fatalf("CacheURL() error = %v", err)
	}
	if putBody != "fake s3 audio bytes" {
		t.Fatalf("uploaded body = %q", putBody)
	}
	if !strings.HasPrefix(putPath, "/flowmusic-cache/flow-assets/") || !strings.HasSuffix(putPath, ".mp3") {
		t.Fatalf("unexpected S3 path: %s", putPath)
	}
	if !strings.HasPrefix(ref.URL, "https://cdn.example.test/cache/flow-assets/") || !strings.HasSuffix(ref.URL, ".mp3") {
		t.Fatalf("unexpected public URL: %+v", ref)
	}
}

func TestCacheURLR2StorageReturnsPresignedURLWithoutPublicBase(t *testing.T) {
	var putPath string
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		if r.Method != http.MethodPut {
			t.Fatalf("unexpected S3 method: %s %s", r.Method, r.URL.String())
		}
		putPath = r.URL.Path
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s3.Close)

	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("fake r2 audio bytes"))
	}))
	t.Cleanup(media.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:                dir,
		CacheDir:               filepath.Join(dir, "tmp"),
		DatabaseDriver:         "sqlite",
		DatabaseURL:            filepath.Join(dir, "flowmusic2api.db"),
		UpstreamTimeout:        time.Second,
		StoragePresignDuration: time.Hour,
	}
	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := db.UpdateCacheConfig(ctx, domain.CacheConfig{
		Enabled:          true,
		StorageMode:      "r2",
		S3Endpoint:       s3.URL,
		S3Bucket:         "flowmusic-cache",
		S3AccessKey:      "access-key",
		S3SecretKey:      "secret-key",
		S3UseSSL:         false,
		S3ForcePathStyle: true,
		S3Prefix:         "flow-assets",
		S3PublicBaseURL:  "https://cdn.example.test/cache",
	}); err != nil {
		t.Fatalf("UpdateCacheConfig() error = %v", err)
	}

	cache := NewCache(cfg, db, media.Client())
	ref, err := cache.CacheURL(ctx, media.URL+"/song.mp3")
	if err != nil {
		t.Fatalf("CacheURL() error = %v", err)
	}
	if !strings.HasPrefix(putPath, "/flowmusic-cache/flow-assets/") || !strings.HasSuffix(putPath, ".mp3") {
		t.Fatalf("unexpected R2 path: %s", putPath)
	}
	if !strings.HasPrefix(ref.URL, s3.URL+"/flowmusic-cache/flow-assets/") || !strings.Contains(ref.URL, "X-Amz-Signature=") {
		t.Fatalf("unexpected presigned URL: %+v", ref)
	}
}

func TestCacheURLS3FailureReturnsErrorWithoutCachedURL(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		http.Error(w, "upload failed", http.StatusInternalServerError)
	}))
	t.Cleanup(s3.Close)

	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("fake failed upload bytes"))
	}))
	t.Cleanup(media.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:         dir,
		CacheDir:        filepath.Join(dir, "tmp"),
		DatabaseDriver:  "sqlite",
		DatabaseURL:     filepath.Join(dir, "flowmusic2api.db"),
		UpstreamTimeout: time.Second,
	}
	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := db.UpdateCacheConfig(ctx, domain.CacheConfig{
		Enabled:          true,
		StorageMode:      "s3",
		S3Endpoint:       s3.URL,
		S3Bucket:         "flowmusic-cache",
		S3UseSSL:         false,
		S3ForcePathStyle: true,
		S3Prefix:         "flow-assets",
		S3PublicBaseURL:  "https://cdn.example.test/cache",
	}); err != nil {
		t.Fatalf("UpdateCacheConfig() error = %v", err)
	}

	sourceURL := media.URL + "/song.mp3"
	cache := NewCache(cfg, db, media.Client())
	ref, err := cache.CacheURL(ctx, sourceURL)
	if err == nil {
		t.Fatalf("CacheURL() error = nil, want upload error")
	}
	if ref.URL != sourceURL || strings.HasPrefix(ref.URL, "https://cdn.example.test/cache/") {
		t.Fatalf("failed upload should keep original URL, got %+v", ref)
	}
}

func TestCacheURLDetectsContentTypeFromTempFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		DataDir:         dir,
		CacheDir:        filepath.Join(dir, "tmp"),
		DatabaseDriver:  "sqlite",
		DatabaseURL:     filepath.Join(dir, "flowmusic2api.db"),
		UpstreamTimeout: time.Second,
	}
	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := db.UpdateCacheConfig(ctx, domain.CacheConfig{
		Enabled:     true,
		StorageMode: "local",
		BaseURL:     "https://cdn.example.test",
	}); err != nil {
		t.Fatalf("UpdateCacheConfig() error = %v", err)
	}

	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(png)),
				Request:    r,
			}, nil
		}),
	}

	cache := NewCache(cfg, db, client)
	ref, err := cache.CacheURL(ctx, "https://media.example.test/cover")
	if err != nil {
		t.Fatalf("CacheURL() error = %v", err)
	}
	if ref.ContentType != "image/png" {
		t.Fatalf("ContentType = %q, want image/png", ref.ContentType)
	}
	if ref.Size != int64(len(png)) {
		t.Fatalf("Size = %d, want %d", ref.Size, len(png))
	}
	if !strings.HasSuffix(ref.URL, ".png") {
		t.Fatalf("URL = %q, want .png suffix", ref.URL)
	}
}

func TestCacheURLUsesMediaProxyForDownload(t *testing.T) {
	var proxiedURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxiedURL = r.URL.String()
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("proxied audio bytes"))
	}))
	t.Cleanup(proxy.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:         dir,
		CacheDir:        filepath.Join(dir, "tmp"),
		DatabaseDriver:  "sqlite",
		DatabaseURL:     filepath.Join(dir, "flowmusic2api.db"),
		UpstreamTimeout: time.Second,
	}
	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := db.UpdateCacheConfig(ctx, domain.CacheConfig{
		Enabled:     true,
		StorageMode: "local",
		BaseURL:     "https://cdn.example.test",
	}); err != nil {
		t.Fatalf("UpdateCacheConfig() error = %v", err)
	}
	if err := db.UpdateProxyConfig(ctx, domain.ProxyConfig{
		MediaProxyEnabled: true,
		MediaProxyURL:     proxy.URL,
	}); err != nil {
		t.Fatalf("UpdateProxyConfig() error = %v", err)
	}
	failingClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("base media client should not be used when media proxy is enabled")
		}),
	}

	cache := NewCache(cfg, db, failingClient)
	sourceURL := "http://media.example.test/song.mp3"
	ref, err := cache.CacheURL(ctx, sourceURL)
	if err != nil {
		t.Fatalf("CacheURL() error = %v", err)
	}
	if proxiedURL != sourceURL {
		t.Fatalf("media proxy saw URL %q, want %q", proxiedURL, sourceURL)
	}
	if ref.OriginalURL != sourceURL || !strings.HasPrefix(ref.URL, "https://cdn.example.test/tmp/") || ref.Size != int64(len("proxied audio bytes")) {
		t.Fatalf("unexpected proxied media ref: %+v", ref)
	}
}

func TestS3TransportUsesMediaProxy(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
	)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.String())
		mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Query().Has("location") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`))
			return
		}
		switch r.Method {
		case http.MethodPut:
			w.Header().Set("ETag", `"test-etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	t.Cleanup(proxy.Close)

	dir := t.TempDir()
	cfg := config.Config{
		DataDir:                dir,
		CacheDir:               filepath.Join(dir, "tmp"),
		DatabaseDriver:         "sqlite",
		DatabaseURL:            filepath.Join(dir, "flowmusic2api.db"),
		UpstreamTimeout:        time.Second,
		StoragePresignDuration: time.Hour,
	}
	ctx := context.Background()
	db, err := store.New(ctx, cfg)
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		t.Fatalf("EnsureDefaults() error = %v", err)
	}
	if err := db.UpdateProxyConfig(ctx, domain.ProxyConfig{
		MediaProxyEnabled: true,
		MediaProxyURL:     proxy.URL,
	}); err != nil {
		t.Fatalf("UpdateProxyConfig() error = %v", err)
	}

	cache := NewCache(cfg, db, http.DefaultClient)
	ref, err := cache.TestConfig(ctx, &domain.CacheConfig{
		Enabled:          true,
		StorageMode:      "r2",
		S3Endpoint:       "http://s3.example.test",
		S3Bucket:         "flowmusic-cache",
		S3AccessKey:      "access-key",
		S3SecretKey:      "secret-key",
		S3UseSSL:         false,
		S3ForcePathStyle: true,
		S3Prefix:         "health",
		S3PublicBaseURL:  "https://cdn.example.test/cache",
	})
	if err != nil {
		t.Fatalf("TestConfig() error = %v", err)
	}
	if !strings.HasPrefix(ref.URL, "http://s3.example.test/flowmusic-cache/health/.flowmusic2api-healthcheck/") || !strings.Contains(ref.URL, "X-Amz-Signature=") {
		t.Fatalf("unexpected test ref URL: %+v", ref)
	}

	mu.Lock()
	gotRequests := append([]string(nil), requests...)
	mu.Unlock()
	if len(gotRequests) == 0 {
		t.Fatalf("S3/R2 proxy saw no requests")
	}
	var sawPut bool
	for _, req := range gotRequests {
		if !strings.Contains(req, "http://s3.example.test/flowmusic-cache/") {
			t.Fatalf("S3/R2 request did not use configured endpoint through proxy: %q (all: %#v)", req, gotRequests)
		}
		if strings.HasPrefix(req, http.MethodPut+" ") {
			sawPut = true
		}
	}
	if !sawPut {
		t.Fatalf("S3/R2 proxy did not see PutObject request: %#v", gotRequests)
	}
}

func TestNormalizeS3Endpoint(t *testing.T) {
	host, secure, err := normalizeS3Endpoint("https://example.r2.cloudflarestorage.com", false)
	if err != nil {
		t.Fatalf("normalizeS3Endpoint() error = %v", err)
	}
	if host != "example.r2.cloudflarestorage.com" || !secure {
		t.Fatalf("unexpected normalized endpoint: host=%q secure=%v", host, secure)
	}
	host, secure, err = normalizeS3Endpoint("127.0.0.1:9000", false)
	if err != nil {
		t.Fatalf("normalizeS3Endpoint(host) error = %v", err)
	}
	if host != "127.0.0.1:9000" || secure {
		t.Fatalf("unexpected host endpoint: host=%q secure=%v", host, secure)
	}
	if _, _, err := normalizeS3Endpoint("https://example.test/path", true); err == nil {
		t.Fatalf("normalizeS3Endpoint() error = nil, want path error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
