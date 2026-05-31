package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
	"flowmusic2api/internal/storage"
	"flowmusic2api/internal/store"
)

type GenerationService struct {
	cfg      config.Config
	db       *store.Store
	accounts *AccountService
	client   *FlowMusicClient
	cache    *storage.Cache
}

type GenerationOutput struct {
	AccountID    int64        `json:"account_id"`
	JobID        string       `json:"job_id"`
	OperationIDs []string     `json:"operation_ids,omitempty"`
	ClipIDs      []string     `json:"clip_ids"`
	Clips        []ClipOutput `json:"clips"`
	RawEvents    []string     `json:"raw_events,omitempty"`
	GeneratedRaw string       `json:"generated_raw,omitempty"`
}

type ClipOutput struct {
	ID              string           `json:"id"`
	Title           string           `json:"title"`
	Audio           domain.MediaRef  `json:"audio"`
	Wav             *domain.MediaRef `json:"wav,omitempty"`
	Image           *domain.MediaRef `json:"image,omitempty"`
	Video           *domain.MediaRef `json:"video,omitempty"`
	Lyrics          string           `json:"lyrics,omitempty"`
	LyricsID        string           `json:"lyrics_id,omitempty"`
	SoundPrompt     string           `json:"sound_prompt,omitempty"`
	OperationID     string           `json:"operation_id,omitempty"`
	OperationType   string           `json:"operation_type,omitempty"`
	DurationSeconds float64          `json:"duration_seconds,omitempty"`
	CreatedAt       string           `json:"created_at,omitempty"`
}

type GenerationProgress struct {
	Stage    string
	Message  string
	Progress int
}

type ProgressFunc func(GenerationProgress)

type GenerationResultLookup struct {
	AccountID    int64
	JobID        string
	OperationIDs []string
	ClipIDs      []string
}

func NewGenerationService(cfg config.Config, db *store.Store, accounts *AccountService, client *FlowMusicClient, cache *storage.Cache) *GenerationService {
	return &GenerationService{cfg: cfg, db: db, accounts: accounts, client: client, cache: cache}
}

func (s *GenerationService) Generate(ctx context.Context, prompt, model string) (GenerationOutput, error) {
	return s.GenerateWithProgress(ctx, prompt, model, nil)
}

func (s *GenerationService) GenerateWithProgress(ctx context.Context, prompt, model string, onProgress ProgressFunc) (GenerationOutput, error) {
	var output GenerationOutput
	emit := func(stage, message string, progress int) {
		if onProgress == nil || strings.TrimSpace(message) == "" {
			return
		}
		onProgress(GenerationProgress{Stage: stage, Message: message, Progress: progress})
	}
	runtime := s.effectiveGenerationRuntimeConfig(ctx)
	if runtime.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runtime.Timeout)
		defer cancel()
	}
	emit("started", "音乐生成任务已启动", 0)
	excludedAccounts := map[int64]struct{}{}
	var lastAuthErr error
	for {
		output = GenerationOutput{}
		emit("account_selecting", "选择可用 FlowMusic 账号...", 3)
		account, releaseAccount, err := s.accounts.AcquireLeaseExcluding(ctx, excludedAccounts, runtime.Timeout)
		if err != nil {
			if lastAuthErr != nil {
				return output, fmt.Errorf("no fallback FlowMusic account after authentication failure: %w", lastAuthErr)
			}
			return output, err
		}
		defer releaseAccount()
		output.AccountID = account.ID
		start := time.Now()
		reqPayload, _ := json.Marshal(map[string]any{"prompt": prompt, "model": model})
		logID, _ := s.db.CreateRequestLog(ctx, domain.RequestLog{
			AccountID:   &account.ID,
			Operation:   "music.generate",
			RequestBody: string(reqPayload),
			StatusCode:  102,
			StatusText:  "running",
			Progress:    1,
		})

		shouldFallback := func(err error) bool {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				emit("request_stopped", fmt.Sprintf("生成请求已结束: %s", err.Error()), 100)
				s.failLog(ctx, logID, account.ID, reqPayload, start, err)
				_ = s.db.TouchAccountUsed(context.Background(), account.ID)
				return false
			}
			emit("account_failed", fmt.Sprintf("账号 %d 生成失败: %s", account.ID, err.Error()), 0)
			s.failLog(ctx, logID, account.ID, reqPayload, start, err)
			s.recordFailure(ctx, account.ID)
			_ = s.db.TouchAccountUsed(context.Background(), account.ID)
			if !isAuthFailure(err) {
				return false
			}
			emit("account_fallback", "认证失败，切换下一个账号重试...", 5)
			lastAuthErr = err
			excludedAccounts[account.ID] = struct{}{}
			releaseAccount()
			return true
		}

		emit("conversation_starting", "提交 FlowMusic 音乐生成请求...", 10)
		jobID, err := retryValueUnless(ctx, runtime.MaxAttempts, isAuthFailure, func() (string, error) {
			return s.client.StartConversation(ctx, *account, prompt, model)
		})
		if err != nil {
			if shouldFallback(err) {
				continue
			}
			return output, err
		}
		output.JobID = jobID
		emit("conversation_started", fmt.Sprintf("FlowMusic 任务已创建: %s", jobID), 25)
		_ = s.db.UpdateRequestLog(ctx, logID, domain.RequestLog{
			AccountID:   &account.ID,
			RequestBody: string(reqPayload),
			StatusCode:  102,
			StatusText:  "streaming",
			Progress:    30,
		})

		emit("streaming", "等待上游流式工具调用...", 30)
		stream, err := retryValueUnless(ctx, runtime.MaxAttempts, isAuthFailure, func() (ConversationResult, error) {
			return s.client.StreamMessagesWithEvents(ctx, *account, jobID, func(event ConversationStreamEvent) {
				for _, message := range event.ProgressMessages() {
					emit("upstream_stream", message, 35)
				}
			})
		})
		if err != nil {
			if shouldFallback(err) {
				continue
			}
			return output, err
		}
		output.RawEvents = stream.RawEvents
		output.OperationIDs = append(output.OperationIDs, stream.OperationIDs...)
		output.ClipIDs = append(output.ClipIDs, stream.ClipIDs...)
		_ = s.db.UpdateRequestLog(ctx, logID, domain.RequestLog{
			AccountID: &account.ID, StatusCode: 102, StatusText: "generating", Progress: 50,
		})
		if len(output.ClipIDs) == 0 {
			if len(stream.OperationIDs) == 0 {
				err := noAudioToolCallError(stream)
				emit("upstream_no_tool", err.Error(), 55)
				s.failLog(ctx, logID, account.ID, reqPayload, start, err)
				return output, err
			}
			emit("polling", "流式事件未直接返回 clip，按 operation_id 轮询歌曲生成状态...", 50)
			deadline := time.Now().Add(runtime.PollTimeout)
			clipIDs, err := s.client.PollClipsWithProgress(ctx, *account, stream.OperationIDs, deadline, func(status ClipPollStatus) {
				emit("polling", status.ProgressMessage(), 55)
			})
			if err != nil {
				if shouldFallback(err) {
					continue
				}
				return output, err
			}
			output.ClipIDs = clipIDs
		}
		_ = s.db.UpdateRequestLog(ctx, logID, domain.RequestLog{
			AccountID: &account.ID, StatusCode: 102, StatusText: "processing", Progress: 70,
		})
		emit("clips_loading", "获取歌曲素材信息...", 70)
		clips, err := retryValueUnless(ctx, runtime.MaxAttempts, isAuthFailure, func() ([]ClipResult, error) {
			return s.client.GetClips(ctx, *account, output.ClipIDs)
		})
		if err != nil {
			if shouldFallback(err) {
				continue
			}
			return output, err
		}
		if len(clips) == 0 {
			err := fmt.Errorf("flowmusic returned no clips")
			s.failLog(ctx, logID, account.ID, reqPayload, start, err)
			s.recordFailure(ctx, account.ID)
			return output, err
		}
		hasAudio := false
		for _, clip := range clips {
			if clip.AudioURL == "" && clip.WavURL == "" {
				continue
			}
			item := newClipOutput(clip)
			if clip.AudioURL != "" {
				emit("caching", fmt.Sprintf("缓存音频文件: %s", firstNonEmpty(clip.Title, clip.ID)), 82)
				ref, err := retryValue(ctx, runtime.MaxAttempts, func() (domain.MediaRef, error) {
					return s.cache.CacheURL(ctx, clip.AudioURL)
				})
				if err != nil {
					cacheErr := fmt.Errorf("cache audio: %w", err)
					s.failLog(ctx, logID, account.ID, reqPayload, start, cacheErr)
					s.recordFailure(ctx, account.ID)
					return output, cacheErr
				}
				item.Audio = ref
			}
			if clip.WavURL != "" {
				emit("caching", fmt.Sprintf("缓存 WAV 文件: %s", firstNonEmpty(clip.Title, clip.ID)), 86)
				ref, err := retryValue(ctx, runtime.MaxAttempts, func() (domain.MediaRef, error) {
					return s.cache.CacheURL(ctx, clip.WavURL)
				})
				if err != nil {
					cacheErr := fmt.Errorf("cache wav: %w", err)
					s.failLog(ctx, logID, account.ID, reqPayload, start, cacheErr)
					s.recordFailure(ctx, account.ID)
					return output, cacheErr
				}
				item.Wav = &ref
			}
			hasAudio = true
			if clip.ImageURL != "" {
				emit("caching", fmt.Sprintf("缓存封面: %s", firstNonEmpty(clip.Title, clip.ID)), 90)
				ref, err := retryValue(ctx, runtime.MaxAttempts, func() (domain.MediaRef, error) {
					return s.cache.CacheURL(ctx, clip.ImageURL)
				})
				if err != nil {
					cacheErr := fmt.Errorf("cache image: %w", err)
					s.failLog(ctx, logID, account.ID, reqPayload, start, cacheErr)
					s.recordFailure(ctx, account.ID)
					return output, cacheErr
				}
				item.Image = &ref
			}
			if clip.VideoURL != "" {
				emit("caching", fmt.Sprintf("缓存视频: %s", firstNonEmpty(clip.Title, clip.ID)), 92)
				ref, err := retryValue(ctx, runtime.MaxAttempts, func() (domain.MediaRef, error) {
					return s.cache.CacheURL(ctx, clip.VideoURL)
				})
				if err != nil {
					cacheErr := fmt.Errorf("cache video: %w", err)
					s.failLog(ctx, logID, account.ID, reqPayload, start, cacheErr)
					s.recordFailure(ctx, account.ID)
					return output, cacheErr
				}
				item.Video = &ref
			}
			output.Clips = append(output.Clips, item)
		}
		if !hasAudio {
			err := fmt.Errorf("flowmusic returned clips without audio")
			s.failLog(ctx, logID, account.ID, reqPayload, start, err)
			s.recordFailure(ctx, account.ID)
			return output, err
		}
		respPayload, _ := json.Marshal(output)
		_ = s.db.UpdateRequestLog(ctx, logID, domain.RequestLog{
			AccountID:       &account.ID,
			RequestBody:     string(reqPayload),
			ResponseBody:    string(respPayload),
			ResponseExcerpt: truncate(string(respPayload), 2048),
			StatusCode:      200,
			DurationMS:      time.Since(start).Milliseconds(),
			StatusText:      "success",
			Progress:        100,
		})
		_ = s.db.IncrementAccountStat(ctx, account.ID, "music", true)
		_ = s.db.TouchAccountUsed(context.Background(), account.ID)
		emit("completed", "音乐生成完成，准备返回结果", 100)
		return output, nil
	}
}

func (s *GenerationService) LookupResult(ctx context.Context, lookup GenerationResultLookup, onProgress ProgressFunc) (GenerationOutput, error) {
	var output GenerationOutput
	emit := func(stage, message string, progress int) {
		if onProgress == nil || strings.TrimSpace(message) == "" {
			return
		}
		onProgress(GenerationProgress{Stage: stage, Message: message, Progress: progress})
	}
	runtime := s.effectiveGenerationRuntimeConfig(ctx)
	if runtime.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, runtime.Timeout)
		defer cancel()
	}
	lookup.JobID = strings.TrimSpace(lookup.JobID)
	lookup.OperationIDs = uniqueStrings(lookup.OperationIDs)
	lookup.ClipIDs = uniqueStrings(lookup.ClipIDs)
	if lookup.JobID == "" && len(lookup.OperationIDs) == 0 && len(lookup.ClipIDs) == 0 {
		return output, fmt.Errorf("job_id, operation_ids, or clip_ids is required")
	}

	account, release, err := s.lookupAccount(ctx, lookup.AccountID, runtime.Timeout)
	if err != nil {
		return output, err
	}
	defer release()
	output.AccountID = account.ID
	output.JobID = lookup.JobID
	output.OperationIDs = append(output.OperationIDs, lookup.OperationIDs...)
	output.ClipIDs = append(output.ClipIDs, lookup.ClipIDs...)

	if len(output.ClipIDs) == 0 {
		pollIDs := append([]string{}, lookup.OperationIDs...)
		if lookup.JobID != "" {
			emit("streaming", "读取 FlowMusic 历史流式结果...", 20)
			stream, err := s.client.StreamMessagesWithEvents(ctx, *account, lookup.JobID, func(event ConversationStreamEvent) {
				for _, message := range event.ProgressMessages() {
					emit("upstream_stream", message, 35)
				}
			})
			if err != nil {
				return output, err
			}
			output.RawEvents = stream.RawEvents
			output.OperationIDs = uniqueStrings(append(output.OperationIDs, stream.OperationIDs...))
			output.ClipIDs = uniqueStrings(append(output.ClipIDs, stream.ClipIDs...))
			pollIDs = uniqueStrings(append(pollIDs, stream.OperationIDs...))
		}
		if len(output.ClipIDs) == 0 {
			if len(pollIDs) == 0 {
				return output, noAudioToolCallError(ConversationResult{JobID: lookup.JobID, RawEvents: output.RawEvents})
			}
			emit("polling", "开始后查 FlowMusic 生成结果...", 30)
			clipIDs, err := s.client.PollClipsWithProgress(ctx, *account, pollIDs, time.Now().Add(runtime.PollTimeout), func(status ClipPollStatus) {
				emit("polling", status.ProgressMessage(), 50)
			})
			if err != nil {
				return output, err
			}
			output.ClipIDs = clipIDs
		}
	}
	if len(output.ClipIDs) == 0 {
		return output, fmt.Errorf("flowmusic lookup returned no clip ids")
	}

	emit("clips_loading", "获取歌曲素材信息...", 70)
	clips, err := retryValueUnless(ctx, runtime.MaxAttempts, isAuthFailure, func() ([]ClipResult, error) {
		return s.client.GetClips(ctx, *account, output.ClipIDs)
	})
	if err != nil {
		return output, err
	}
	if len(clips) == 0 {
		return output, fmt.Errorf("flowmusic returned no clips")
	}
	hasAudio := false
	for _, clip := range clips {
		if clip.AudioURL == "" && clip.WavURL == "" {
			continue
		}
		item := newClipOutput(clip)
		if clip.AudioURL != "" {
			emit("caching", fmt.Sprintf("缓存音频文件: %s", firstNonEmpty(clip.Title, clip.ID)), 82)
			ref, err := retryValue(ctx, runtime.MaxAttempts, func() (domain.MediaRef, error) {
				return s.cache.CacheURL(ctx, clip.AudioURL)
			})
			if err != nil {
				return output, fmt.Errorf("cache audio: %w", err)
			}
			item.Audio = ref
		}
		if clip.WavURL != "" {
			emit("caching", fmt.Sprintf("缓存 WAV 文件: %s", firstNonEmpty(clip.Title, clip.ID)), 86)
			ref, err := retryValue(ctx, runtime.MaxAttempts, func() (domain.MediaRef, error) {
				return s.cache.CacheURL(ctx, clip.WavURL)
			})
			if err != nil {
				return output, fmt.Errorf("cache wav: %w", err)
			}
			item.Wav = &ref
		}
		hasAudio = true
		if clip.ImageURL != "" {
			emit("caching", fmt.Sprintf("缓存封面: %s", firstNonEmpty(clip.Title, clip.ID)), 90)
			ref, err := retryValue(ctx, runtime.MaxAttempts, func() (domain.MediaRef, error) {
				return s.cache.CacheURL(ctx, clip.ImageURL)
			})
			if err != nil {
				return output, fmt.Errorf("cache image: %w", err)
			}
			item.Image = &ref
		}
		if clip.VideoURL != "" {
			emit("caching", fmt.Sprintf("缓存视频: %s", firstNonEmpty(clip.Title, clip.ID)), 92)
			ref, err := retryValue(ctx, runtime.MaxAttempts, func() (domain.MediaRef, error) {
				return s.cache.CacheURL(ctx, clip.VideoURL)
			})
			if err != nil {
				return output, fmt.Errorf("cache video: %w", err)
			}
			item.Video = &ref
		}
		output.Clips = append(output.Clips, item)
	}
	if !hasAudio {
		return output, fmt.Errorf("flowmusic returned clips without audio")
	}
	emit("completed", "后查结果完成", 100)
	return output, nil
}

func newClipOutput(clip ClipResult) ClipOutput {
	return ClipOutput{
		ID:              clip.ID,
		Title:           clip.Title,
		Lyrics:          clip.Lyrics,
		LyricsID:        clip.LyricsID,
		SoundPrompt:     clip.SoundPrompt,
		OperationID:     clip.OperationID,
		OperationType:   clip.OperationType,
		DurationSeconds: clip.DurationSeconds,
		CreatedAt:       clip.CreatedAt,
	}
}

func (s *GenerationService) lookupAccount(ctx context.Context, accountID int64, leaseTTL time.Duration) (*domain.Account, func(), error) {
	if accountID > 0 {
		account, err := s.db.GetAccount(ctx, accountID)
		if err != nil {
			return nil, func() {}, err
		}
		s.accounts.applyDefaultProxy(ctx, account)
		account, err = s.accounts.ensureFreshBearerForManualRequest(ctx, account)
		if err != nil {
			return nil, func() {}, err
		}
		return account, func() {}, nil
	}
	return s.accounts.AcquireLeaseExcluding(ctx, nil, leaseTTL)
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = appendUnique(out, value)
	}
	return out
}

func noAudioToolCallError(stream ConversationResult) error {
	hasToolCall, text := streamAudioDiagnostics(stream)
	sample := ""
	for _, raw := range stream.RawEvents {
		if strings.Contains(raw, "audio__create_song") || strings.Contains(raw, "operation_id") || strings.Contains(raw, "clip_id") {
			if len(raw) > 300 {
				sample = raw[:300] + "..."
			} else {
				sample = raw
			}
			break
		}
	}
	if sample == "" && len(stream.RawEvents) > 0 {
		raw := stream.RawEvents[len(stream.RawEvents)-1]
		if len(raw) > 300 {
			sample = raw[:300] + "..."
		} else {
			sample = raw
		}
	}
	switch {
	case hasToolCall && text != "" && sample != "":
		return fmt.Errorf("flowmusic stream triggered audio__create_song but returned no operation_id or clip_id; raw_sample=%s", sample)
	case hasToolCall && sample != "":
		return fmt.Errorf("flowmusic stream triggered audio__create_song but returned no operation_id or clip_id; raw_sample=%s", sample)
	case hasToolCall:
		return fmt.Errorf("flowmusic stream triggered audio__create_song but returned no operation_id or clip_id")
	case text != "":
		return fmt.Errorf("flowmusic stream returned no audio__create_song tool call; upstream responded as chat instead of generating music")
	default:
		return fmt.Errorf("flowmusic stream returned no audio__create_song tool call and no operation_id/clip_id; job_id=%s", stream.JobID)
	}
}

func streamAudioDiagnostics(stream ConversationResult) (bool, string) {
	var hasToolCall bool
	var texts []string
	for _, raw := range stream.RawEvents {
		event := parseConversationStreamEvent("", raw)
		if event.ToolName == "audio__create_song" && (event.PartKind == "tool-call" || event.PartKind == "tool-return" || event.PartKind == "retry-prompt") {
			hasToolCall = true
		}
		switch event.PartKind {
		case "text":
			if text := firstNonEmpty(event.TextDelta, event.TextContent); text != "" {
				texts = append(texts, text)
			}
		case "retry-prompt":
			if event.TextContent != "" {
				texts = append(texts, event.TextContent)
			}
		}
	}
	return hasToolCall, strings.TrimSpace(strings.Join(texts, ""))
}

type generationRuntimeConfig struct {
	Timeout     time.Duration
	PollTimeout time.Duration
	MaxAttempts int
}

func (s *GenerationService) effectiveGenerationRuntimeConfig(ctx context.Context) generationRuntimeConfig {
	cfg := generationRuntimeConfig{
		Timeout:     s.cfg.GenerationTimeout,
		PollTimeout: s.cfg.GenerationTimeout,
		MaxAttempts: 1,
	}
	if dbCfg, err := s.db.GetGenerationConfig(ctx); err == nil && dbCfg != nil {
		if dbCfg.Timeout > 0 {
			cfg.Timeout = time.Duration(dbCfg.Timeout) * time.Second
		}
		if dbCfg.VideoTimeout > 0 {
			cfg.PollTimeout = time.Duration(dbCfg.VideoTimeout) * time.Second
		}
		if dbCfg.MaxRetries > 0 {
			cfg.MaxAttempts = dbCfg.MaxRetries
		}
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = cfg.Timeout
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 1
	}
	return cfg
}

func retryValue[T any](ctx context.Context, attempts int, fn func() (T, error)) (T, error) {
	return retryValueUnless(ctx, attempts, nil, fn)
}

func retryValueUnless[T any](ctx context.Context, attempts int, stopRetry func(error) bool, fn func() (T, error)) (T, error) {
	var zero T
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	backoff := 500 * time.Millisecond
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		value, err := fn()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if stopRetry != nil && stopRetry(err) {
			break
		}
		if i == attempts-1 {
			break
		}
		timer := time.NewTimer(backoff)
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
	}
	return zero, lastErr
}

func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var upstreamErr *upstreamHTTPError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.StatusCode == http.StatusUnauthorized || upstreamErr.StatusCode == http.StatusForbidden
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "http 401") || strings.Contains(text, "http 403") || strings.Contains(text, "unauthorized") || strings.Contains(text, "forbidden")
}

func (s *GenerationService) recordFailure(ctx context.Context, accountID int64) {
	if err := s.db.IncrementAccountStat(ctx, accountID, "music", false); err != nil {
		return
	}
	threshold := 3
	if cfg, err := s.db.GetAdminConfig(ctx); err == nil && cfg != nil && cfg.ErrorBan > 0 {
		threshold = cfg.ErrorBan
	}
	account, err := s.db.GetAccount(ctx, accountID)
	if err != nil || account.ConsecutiveErrorCount < threshold {
		return
	}
	_ = s.db.SetAccountActive(ctx, accountID, false)
}

func (s *GenerationService) failLog(ctx context.Context, logID, accountID int64, reqPayload []byte, start time.Time, err error) {
	errText := err.Error()
	resp, _ := json.Marshal(map[string]any{"error": errText})
	statusCode := 500
	statusText := "failed"
	if errors.Is(err, context.Canceled) {
		statusCode = 499
		statusText = "canceled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		statusCode = 504
		statusText = "timeout"
	}
	writeCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		writeCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	_ = s.db.UpdateRequestLog(writeCtx, logID, domain.RequestLog{
		AccountID:       &accountID,
		RequestBody:     string(reqPayload),
		ResponseBody:    string(resp),
		ResponseExcerpt: truncate(string(resp), 2048),
		StatusCode:      statusCode,
		DurationMS:      time.Since(start).Milliseconds(),
		StatusText:      statusText,
		Progress:        100,
		ErrorSummary:    errText,
	})
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + fmt.Sprintf("... [truncated %d bytes]", len(value)-max)
}
