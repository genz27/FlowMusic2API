package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
	"flowmusic2api/internal/store"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Cache struct {
	cfg        config.Config
	store      *store.Store
	httpClient *http.Client
}

const maxCacheBytes int64 = 1024 * 1024 * 1024
const cacheHealthcheckContent = "flowmusic2api cache healthcheck\n"

func NewCache(cfg config.Config, db *store.Store, httpClient *http.Client) *Cache {
	return &Cache{cfg: cfg, store: db, httpClient: httpClient}
}

func (c *Cache) CacheURL(ctx context.Context, sourceURL string) (domain.MediaRef, error) {
	sourceURL = strings.TrimSpace(sourceURL)
	ref := domain.MediaRef{OriginalURL: sourceURL, URL: sourceURL}
	if sourceURL == "" {
		return ref, nil
	}
	cacheCfg, err := c.store.GetCacheConfig(ctx)
	if err != nil || cacheCfg == nil || !cacheCfg.Enabled {
		return ref, err
	}
	client := c.httpClient
	if proxyCfg, err := c.store.GetProxyConfig(ctx); err == nil && proxyCfg != nil && proxyCfg.MediaProxyEnabled && strings.TrimSpace(proxyCfg.MediaProxyURL) != "" {
		client = newHTTPClient(c.cfg, proxyCfg.MediaProxyURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return ref, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return ref, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ref, fmt.Errorf("download media: HTTP %d", resp.StatusCode)
	}
	tmp, err := c.createTempFile()
	if err != nil {
		return ref, err
	}
	tmpPath := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpPath)
	}()
	closeTemp := func() error {
		if closed {
			return nil
		}
		closed = true
		return tmp.Close()
	}

	size, err := io.Copy(tmp, io.LimitReader(resp.Body, maxCacheBytes+1))
	if err != nil {
		return ref, err
	}
	if size > maxCacheBytes {
		return ref, fmt.Errorf("media exceeds cache size limit: %d bytes", maxCacheBytes)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType, err = detectFileContentType(tmp)
		if err != nil {
			return ref, err
		}
	}
	ref.ContentType = contentType
	ref.Size = size
	ext := extensionFromURL(sourceURL, contentType)
	hash := sha256.Sum256([]byte(sourceURL + ":" + fmt.Sprint(size)))
	name := hex.EncodeToString(hash[:]) + ext

	switch strings.ToLower(strings.TrimSpace(cacheCfg.StorageMode)) {
	case "s3", "r2":
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return ref, err
		}
		return c.uploadS3(ctx, cacheCfg, name, contentType, tmp, size, ref)
	default:
		if err := closeTemp(); err != nil {
			return ref, err
		}
		return c.saveLocal(cacheCfg, name, tmpPath, ref)
	}
}

func (c *Cache) createTempFile() (*os.File, error) {
	dir := strings.TrimSpace(c.cfg.CacheDir)
	if dir == "" {
		return os.CreateTemp("", "flowmusic-cache-*")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, ".flowmusic-cache-*")
}

func detectFileContentType(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	var header [512]byte
	n, err := file.Read(header[:])
	if err != nil && err != io.EOF {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return http.DetectContentType(header[:n]), nil
}

func (c *Cache) saveLocal(cacheCfg *domain.CacheConfig, name, tmpPath string, ref domain.MediaRef) (domain.MediaRef, error) {
	if err := os.MkdirAll(c.cfg.CacheDir, 0o755); err != nil {
		return ref, err
	}
	fullPath := filepath.Join(c.cfg.CacheDir, name)
	if err := replaceFile(tmpPath, fullPath); err != nil {
		return ref, err
	}
	baseURL := strings.TrimRight(firstNonEmpty(cacheCfg.BaseURL, cacheCfg.S3PublicBaseURL), "/")
	if baseURL != "" {
		ref.URL = baseURL + "/tmp/" + url.PathEscape(name)
	} else {
		ref.URL = "/tmp/" + url.PathEscape(name)
	}
	ref.LocalURL = ref.URL
	return ref, nil
}

func (c *Cache) TestConfig(ctx context.Context, cacheCfg *domain.CacheConfig) (domain.MediaRef, error) {
	ref := domain.MediaRef{ContentType: "text/plain; charset=utf-8", Size: int64(len(cacheHealthcheckContent))}
	if cacheCfg == nil {
		return ref, fmt.Errorf("cache config is nil")
	}
	switch strings.ToLower(strings.TrimSpace(cacheCfg.StorageMode)) {
	case "s3", "r2":
		client, bucket, err := c.s3Client(ctx, cacheCfg)
		if err != nil {
			return ref, err
		}
		objectName := s3ObjectName(cacheCfg, path.Join(".flowmusic2api-healthcheck", fmt.Sprintf("healthcheck-%d.txt", time.Now().UnixNano())))
		_, err = client.PutObject(ctx, bucket, objectName, bytes.NewReader([]byte(cacheHealthcheckContent)), int64(len(cacheHealthcheckContent)), minio.PutObjectOptions{ContentType: ref.ContentType})
		if err != nil {
			return ref, err
		}
		defer func() {
			_ = client.RemoveObject(context.Background(), bucket, objectName, minio.RemoveObjectOptions{})
		}()
		ref.OriginalURL = objectName
		publicBase := objectStoragePublicBase(cacheCfg)
		if publicBase != "" {
			ref.URL = publicBase + "/" + pathEscapeSegments(objectName)
		} else {
			presigned, err := client.PresignedGetObject(ctx, bucket, objectName, c.cfg.StoragePresignDuration, nil)
			if err != nil {
				return ref, err
			}
			ref.URL = presigned.String()
		}
		ref.LocalURL = ref.URL
		return ref, nil
	default:
		tmp, err := c.createHealthcheckFile()
		if err != nil {
			return ref, err
		}
		tmpPath := tmp.Name()
		if _, err := tmp.WriteString(cacheHealthcheckContent); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return ref, err
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return ref, err
		}
		ref.OriginalURL = tmpPath
		name := filepath.Base(tmpPath)
		baseURL := strings.TrimRight(firstNonEmpty(cacheCfg.BaseURL, cacheCfg.S3PublicBaseURL), "/")
		if baseURL != "" {
			ref.URL = baseURL + "/tmp/" + url.PathEscape(name)
		} else {
			ref.URL = "/tmp/" + url.PathEscape(name)
		}
		ref.LocalURL = ref.URL
		return ref, nil
	}
}

func (c *Cache) createHealthcheckFile() (*os.File, error) {
	dir := strings.TrimSpace(c.cfg.CacheDir)
	if dir == "" {
		return os.CreateTemp("", ".flowmusic2api-healthcheck-*.txt")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(dir, ".flowmusic2api-healthcheck-*.txt")
}

func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (c *Cache) uploadS3(ctx context.Context, cacheCfg *domain.CacheConfig, name, contentType string, body io.Reader, size int64, ref domain.MediaRef) (domain.MediaRef, error) {
	client, bucket, err := c.s3Client(ctx, cacheCfg)
	if err != nil {
		return ref, err
	}
	objectName := s3ObjectName(cacheCfg, name)
	_, err = client.PutObject(ctx, bucket, objectName, body, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return ref, err
	}
	publicBase := objectStoragePublicBase(cacheCfg)
	if publicBase != "" {
		ref.URL = publicBase + "/" + pathEscapeSegments(objectName)
	} else {
		presigned, err := client.PresignedGetObject(ctx, bucket, objectName, c.cfg.StoragePresignDuration, nil)
		if err != nil {
			return ref, err
		}
		ref.URL = presigned.String()
	}
	ref.LocalURL = ref.URL
	return ref, nil
}

func (c *Cache) s3Client(ctx context.Context, cacheCfg *domain.CacheConfig) (*minio.Client, string, error) {
	endpoint := strings.TrimSpace(cacheCfg.S3Endpoint)
	bucket := strings.TrimSpace(cacheCfg.S3Bucket)
	if endpoint == "" || bucket == "" {
		return nil, "", fmt.Errorf("S3/R2 endpoint and bucket are required")
	}
	endpointHost, secure, err := normalizeS3Endpoint(endpoint, cacheCfg.S3UseSSL)
	if err != nil {
		return nil, "", err
	}
	client, err := minio.New(endpointHost, &minio.Options{
		Creds:        credentials.NewStaticV4(cacheCfg.S3AccessKey, cacheCfg.S3SecretKey, ""),
		Secure:       secure,
		Region:       cacheCfg.S3Region,
		BucketLookup: bucketLookup(cacheCfg.S3ForcePathStyle),
		Transport:    c.s3Transport(ctx),
	})
	if err != nil {
		return nil, "", err
	}
	return client, bucket, nil
}

func objectStoragePublicBase(cacheCfg *domain.CacheConfig) string {
	if cacheCfg == nil || strings.EqualFold(strings.TrimSpace(cacheCfg.StorageMode), "r2") {
		return ""
	}
	return strings.TrimRight(cacheCfg.S3PublicBaseURL, "/")
}

func s3ObjectName(cacheCfg *domain.CacheConfig, name string) string {
	objectName := strings.Trim(strings.TrimSpace(cacheCfg.S3Prefix), "/")
	if objectName != "" {
		objectName += "/"
	}
	return objectName + strings.TrimLeft(name, "/")
}

func (c *Cache) s3Transport(ctx context.Context) http.RoundTripper {
	proxyURL := strings.TrimSpace(c.cfg.DefaultProxyURL)
	if c.store != nil {
		if proxyCfg, err := c.store.GetProxyConfig(ctx); err == nil && proxyCfg != nil {
			if proxyCfg.MediaProxyEnabled && strings.TrimSpace(proxyCfg.MediaProxyURL) != "" {
				proxyURL = strings.TrimSpace(proxyCfg.MediaProxyURL)
			} else if proxyCfg.ProxyEnabled && strings.TrimSpace(proxyCfg.ProxyURL) != "" {
				proxyURL = strings.TrimSpace(proxyCfg.ProxyURL)
			}
		}
	}
	client := newHTTPClient(c.cfg, proxyURL)
	return client.Transport
}

func normalizeS3Endpoint(endpoint string, fallbackSecure bool) (string, bool, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", fallbackSecure, fmt.Errorf("S3/R2 endpoint is required")
	}
	parsed, err := url.Parse(endpoint)
	if err == nil && parsed.Scheme != "" {
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", fallbackSecure, fmt.Errorf("unsupported S3/R2 endpoint scheme %q", parsed.Scheme)
		}
		if parsed.Host == "" {
			return "", fallbackSecure, fmt.Errorf("S3/R2 endpoint host is required")
		}
		if strings.Trim(parsed.Path, "/") != "" {
			return "", fallbackSecure, fmt.Errorf("S3/R2 endpoint must not include a path")
		}
		return parsed.Host, parsed.Scheme == "https", nil
	}
	if strings.Contains(endpoint, "/") {
		return "", fallbackSecure, fmt.Errorf("S3/R2 endpoint must be host[:port] or http(s)://host[:port]")
	}
	return endpoint, fallbackSecure, nil
}

func bucketLookup(forcePathStyle bool) minio.BucketLookupType {
	if forcePathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupAuto
}

func extensionFromURL(rawURL, contentType string) string {
	if parsed, err := url.Parse(rawURL); err == nil {
		ext := strings.ToLower(path.Ext(parsed.Path))
		if ext != "" && len(ext) <= 8 {
			return ext
		}
	}
	if mediaType, _, err := mime.ParseMediaType(contentType); err == nil {
		if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
			return exts[0]
		}
		switch mediaType {
		case "audio/mpeg":
			return ".mp3"
		case "audio/mp4", "audio/x-m4a":
			return ".m4a"
		case "audio/wav", "audio/wave":
			return ".wav"
		case "video/x-msvideo", "video/avi", "video/msvideo":
			return ".avi"
		}
	}
	return ".bin"
}

func pathEscapeSegments(value string) string {
	parts := strings.Split(value, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func newHTTPClient(cfg config.Config, proxyURL string) *http.Client {
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

func CleanupLoop(ctx context.Context, dir string, maxAge time.Duration) {
	if maxAge <= 0 || strings.TrimSpace(dir) == "" {
		return
	}
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
				if err != nil || d == nil || d.IsDir() {
					return nil
				}
				info, err := d.Info()
				if err == nil && time.Since(info.ModTime()) > maxAge {
					_ = os.Remove(p)
				}
				return nil
			})
		}
	}
}

func CleanupLoopWithStore(ctx context.Context, db *store.Store, dir string, fallbackMaxAge time.Duration) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			maxAge := fallbackMaxAge
			if db != nil {
				if cfg, err := db.GetCacheConfig(ctx); err == nil && cfg != nil {
					if !cfg.Enabled || cfg.Timeout == 0 {
						continue
					}
					maxAge = time.Duration(cfg.Timeout) * time.Second
				}
			}
			cleanupDir(dir, maxAge)
		}
	}
}

func cleanupDir(dir string, maxAge time.Duration) {
	if maxAge <= 0 || strings.TrimSpace(dir) == "" {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err == nil && time.Since(info.ModTime()) > maxAge {
			_ = os.Remove(p)
		}
		return nil
	})
}
