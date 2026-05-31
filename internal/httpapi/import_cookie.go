package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type flowMusicCookieChunk struct {
	index int
	value string
}

func importItemsFromFlowMusicCookieExport(cookies []map[string]any) ([]map[string]any, bool, error) {
	groups := map[string][]flowMusicCookieChunk{}
	for _, cookie := range cookies {
		name := stringFromMap(cookie, "name")
		prefix, index, ok := flowMusicAuthCookieName(name)
		if !ok {
			continue
		}
		value := stringFromMap(cookie, "value")
		if value == "" {
			return nil, true, fmt.Errorf("FlowMusic auth cookie %q has empty value", name)
		}
		groups[prefix] = append(groups[prefix], flowMusicCookieChunk{index: index, value: value})
	}
	if len(groups) == 0 {
		return nil, false, nil
	}

	const preferredPrefix = "sb-sb-auth-token."
	chunks, ok := groups[preferredPrefix]
	if !ok {
		if len(groups) > 1 {
			return nil, true, fmt.Errorf("multiple Supabase auth cookie groups found; keep only FlowMusic auth cookies")
		}
		for _, groupChunks := range groups {
			chunks = groupChunks
		}
	}
	item, err := importItemFromFlowMusicCookieChunks(chunks)
	if err != nil {
		return nil, true, err
	}
	return []map[string]any{item}, true, nil
}

func flowMusicAuthCookieName(name string) (string, int, bool) {
	name = strings.TrimSpace(name)
	const marker = "-auth-token."
	if !strings.HasPrefix(name, "sb-") {
		return "", 0, false
	}
	pos := strings.LastIndex(name, marker)
	if pos <= len("sb-") {
		return "", 0, false
	}
	prefix := name[:pos+len(marker)]
	index, err := strconv.Atoi(name[pos+len(marker):])
	if err != nil || index < 0 {
		return "", 0, false
	}
	return prefix, index, true
}

func importItemFromFlowMusicCookieChunks(chunks []flowMusicCookieChunk) (map[string]any, error) {
	if len(chunks) == 0 {
		return nil, fmt.Errorf("FlowMusic auth cookie is missing")
	}
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].index < chunks[j].index })
	seen := map[int]struct{}{}
	var builder strings.Builder
	for i, chunk := range chunks {
		if _, exists := seen[chunk.index]; exists {
			return nil, fmt.Errorf("duplicate FlowMusic auth cookie chunk %d", chunk.index)
		}
		seen[chunk.index] = struct{}{}
		if chunk.index != i {
			return nil, fmt.Errorf("FlowMusic auth cookie chunks must start at 0 and be contiguous")
		}
		builder.WriteString(chunk.value)
	}

	session, err := decodeFlowMusicCookieSession(builder.String())
	if err != nil {
		return nil, err
	}

	user := mapFromMap(session, "user")
	metadata := mapFromMap(user, "user_metadata")
	email := firstNonEmpty(stringFromMap(user, "email"), stringFromMap(metadata, "email"))
	if email == "" {
		email = "flowmusic-cookie@local"
	}
	name := firstNonEmpty(stringFromMap(metadata, "name"), stringFromMap(metadata, "full_name"), stringFromMap(user, "name"))
	refreshToken := stringFromMap(session, "refresh_token")
	providerToken := stringFromMap(session, "provider_token")
	providerRefreshToken := stringFromMap(session, "provider_refresh_token")
	if refreshToken == "" && providerToken == "" && providerRefreshToken == "" {
		return nil, fmt.Errorf("FlowMusic auth cookie session has no refresh_token or provider credentials")
	}

	return map[string]any{
		"email":                  email,
		"name":                   name,
		"remark":                 "imported from FlowMusic browser cookie",
		"protocol_mode":          "refresh_token",
		"refresh_token":          refreshToken,
		"provider_token":         providerToken,
		"provider_refresh_token": providerRefreshToken,
		"auto_refresh_enabled":   true,
		"refresh_interval_minutes": 30,
		"image_enabled":          true,
		"video_enabled":          true,
		"upscale_enabled":        true,
	}, nil
}

func decodeFlowMusicCookieSession(value string) (map[string]any, error) {
	value = strings.TrimSpace(value)
	if unescaped, err := url.PathUnescape(value); err == nil {
		value = unescaped
	}
	value = strings.TrimPrefix(value, "base64-")
	if value == "" {
		return nil, fmt.Errorf("FlowMusic auth cookie is empty")
	}

	payload, err := decodeFlexibleBase64(value)
	if err != nil {
		return nil, fmt.Errorf("decode FlowMusic auth cookie: %w", err)
	}
	var session map[string]any
	if err := json.Unmarshal(payload, &session); err != nil {
		return nil, fmt.Errorf("decode FlowMusic auth cookie session JSON: %w", err)
	}
	return session, nil
}

func decodeFlexibleBase64(value string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	padded := value
	if remainder := len(padded) % 4; remainder != 0 {
		padded += strings.Repeat("=", 4-remainder)
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		if decoded, err := encoding.DecodeString(padded); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("invalid base64 payload")
}

func mapFromMap(m map[string]any, key string) map[string]any {
	value, ok := m[key]
	if !ok || value == nil {
		return nil
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}
