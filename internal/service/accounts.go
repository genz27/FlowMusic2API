package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/domain"
	"flowmusic2api/internal/store"
)

type AccountService struct {
	cfg          config.Config
	db           *store.Store
	client       *FlowMusicClient
	leaseMu      sync.Mutex
	leases       map[int64]time.Time
	refreshMu    sync.Mutex
	refreshLocks map[int64]*sync.Mutex
}

func NewAccountService(cfg config.Config, db *store.Store, client *FlowMusicClient) *AccountService {
	return &AccountService{cfg: cfg, db: db, client: client, leases: map[int64]time.Time{}, refreshLocks: map[int64]*sync.Mutex{}}
}

func (s *AccountService) Start(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.TokenRefreshInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.RunRefreshOnce(ctx)
			}
		}
	}()
}

func (s *AccountService) RunRefreshOnce(ctx context.Context) {
	cfg, err := s.db.GetTokenRefreshConfig(ctx)
	if err != nil || cfg == nil || !cfg.Enabled || !cfg.ATAutoRefreshEnabled {
		return
	}
	accounts, err := s.db.GetActiveAccounts(ctx)
	if err != nil {
		return
	}
	lead := time.Duration(cfg.RefreshBeforeExpiryMs) * time.Second
	if lead <= 0 {
		lead = s.cfg.TokenRefreshLead
	}
	now := time.Now().UTC()
	for _, account := range accounts {
		if s.isLeased(account.ID, now) {
			continue
		}
		if !account.AutoRefreshEnabled || !canRefreshAccount(account) || !shouldRefreshAccount(account, lead, now) {
			continue
		}
		_, _ = s.RefreshAccount(ctx, account.ID)
	}
}

func (s *AccountService) RefreshAccount(ctx context.Context, id int64) (*domain.Account, error) {
	unlock := s.lockAccountRefresh(id)
	defer unlock()
	account, err := s.db.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	s.applyDefaultProxy(ctx, account)
	updated, err := s.refreshAccountByProtocol(ctx, *account)
	now := time.Now().UTC()
	if err != nil {
		fields := map[string]any{
			"last_refresh_at":     now,
			"last_refresh_result": err.Error(),
		}
		if strings.TrimSpace(updated.Email) != "" && updated.Email != account.Email {
			fields["email"] = updated.Email
		}
		if strings.TrimSpace(updated.Name) != "" && updated.Name != account.Name {
			fields["name"] = updated.Name
		}
		if strings.TrimSpace(updated.RefreshToken) != "" && updated.RefreshToken != account.RefreshToken {
			fields["refresh_token"] = updated.RefreshToken
		}
		if strings.TrimSpace(updated.ProviderToken) != "" && updated.ProviderToken != account.ProviderToken {
			fields["provider_token"] = updated.ProviderToken
		}
		if strings.TrimSpace(updated.ProviderRefreshToken) != "" && updated.ProviderRefreshToken != account.ProviderRefreshToken {
			fields["provider_refresh_token"] = updated.ProviderRefreshToken
		}
		if strings.TrimSpace(updated.FlowBearer) != "" && updated.FlowBearer != account.FlowBearer {
			fields["flow_bearer"] = updated.FlowBearer
		}
		if updated.ExpiresAt != nil {
			fields["expires_at"] = nullableTimeArg(updated.ExpiresAt)
		}
		if updated.ATExpires != nil {
			fields["at_expires"] = nullableTimeArg(updated.ATExpires)
		}
		_ = s.db.UpdateAccountFields(ctx, id, fields)
		return account, err
	}
	fields := map[string]any{
		"email":                  firstNonEmpty(updated.Email, account.Email),
		"name":                   updated.Name,
		"refresh_token":          updated.RefreshToken,
		"access_token":           updated.AccessToken,
		"provider_token":         updated.ProviderToken,
		"provider_refresh_token": updated.ProviderRefreshToken,
		"flow_bearer":            updated.FlowBearer,
		"expires_at":             nullableTimeArg(updated.ExpiresAt),
		"at_expires":             nullableTimeArg(updated.ATExpires),
		"last_refresh_at":        now,
		"last_refresh_result":    updated.LastRefreshResult,
	}
	if strings.TrimSpace(updated.Cookies) != "" {
		fields["cookies"] = updated.Cookies
	}
	if err := s.db.UpdateAccountFields(ctx, id, fields); err != nil {
		return nil, err
	}
	updated.ID = id
	return &updated, nil
}

func (s *AccountService) RefreshCredits(ctx context.Context, id int64) (*domain.Account, error) {
	account, err := s.db.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	s.applyDefaultProxy(ctx, account)
	account, err = s.ensureFreshBearerForManualRequest(ctx, account)
	if err != nil {
		return account, err
	}
	info, err := s.client.GetCredits(ctx, *account)
	if err != nil {
		return account, err
	}
	_ = s.db.UpdateAccountFields(ctx, id, map[string]any{
		"credits":           int(info.CreditsRemaining),
		"tokens_remaining":  info.TokensRemaining,
		"subscription_tier": info.SubscriptionTier,
	})
	return s.db.GetAccount(ctx, id)
}

func (s *AccountService) ensureFreshBearerForManualRequest(ctx context.Context, account *domain.Account) (*domain.Account, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	now := time.Now().UTC()
	_, refreshLead := s.tokenRefreshPolicy(ctx)
	needsRefresh := canRefreshAccount(*account) && (!hasCurrentFlowBearer(*account, now) || (account.AutoRefreshEnabled && shouldRefreshAccount(*account, refreshLead, now)))
	if !needsRefresh {
		return account, nil
	}
	refreshed, err := s.RefreshAccount(ctx, account.ID)
	if err != nil {
		if hasCurrentFlowBearer(*account, now) {
			return account, nil
		}
		return account, err
	}
	s.applyDefaultProxy(ctx, refreshed)
	return refreshed, nil
}

func (s *AccountService) Acquire(ctx context.Context) (*domain.Account, error) {
	return s.AcquireExcluding(ctx, nil)
}

func (s *AccountService) AcquireExcluding(ctx context.Context, excluded map[int64]struct{}) (*domain.Account, error) {
	return s.acquireExcluding(ctx, excluded)
}

func (s *AccountService) AcquireLeaseExcluding(ctx context.Context, excluded map[int64]struct{}, leaseTTL time.Duration) (*domain.Account, func(), error) {
	if leaseTTL <= 0 {
		leaseTTL = s.cfg.GenerationTimeout
	}
	if leaseTTL <= 0 {
		leaseTTL = 10 * time.Minute
	}
	baseExcluded := cloneExcluded(excluded)
	for {
		mergedExcluded := cloneExcluded(baseExcluded)
		activeLeases := s.activeLeases(time.Now().UTC())
		for id := range activeLeases {
			mergedExcluded[id] = struct{}{}
		}
		account, err := s.acquireExcluding(ctx, mergedExcluded)
		if err != nil {
			if len(activeLeases) == 0 {
				return nil, func() {}, err
			}
			if waitErr := waitForLeaseRetry(ctx); waitErr != nil {
				return nil, func() {}, waitErr
			}
			continue
		}
		if s.tryLease(account.ID, leaseTTL) {
			var once sync.Once
			release := func() {
				once.Do(func() {
					s.releaseLease(account.ID)
				})
			}
			return account, release, nil
		}
		if err := waitForLeaseRetry(ctx); err != nil {
			return nil, func() {}, err
		}
	}
}

func (s *AccountService) acquireExcluding(ctx context.Context, excluded map[int64]struct{}) (*domain.Account, error) {
	accounts, err := s.db.GetActiveAccounts(ctx)
	if err != nil {
		return nil, err
	}
	accounts = s.orderAccountsForCallMode(ctx, accounts)
	refreshEnabled, refreshLead := s.tokenRefreshPolicy(ctx)
	var lastErr error
	now := time.Now().UTC()
	for _, account := range accounts {
		if _, skip := excluded[account.ID]; skip {
			continue
		}
		s.applyDefaultProxy(ctx, &account)
		if refreshEnabled && account.AutoRefreshEnabled && canRefreshAccount(account) && shouldRefreshAccount(account, refreshLead, now) {
			refreshed, err := s.RefreshAccount(ctx, account.ID)
			if err != nil {
				lastErr = err
				if hasCurrentFlowBearer(account, now) {
					return &account, nil
				}
				continue
			}
			if refreshed != nil && hasCurrentFlowBearer(*refreshed, now) {
				return refreshed, nil
			}
			continue
		}
		if hasCurrentFlowBearer(account, now) {
			return &account, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("no active FlowMusic account with usable Bearer token; last refresh error: %w", lastErr)
	}
	return nil, fmt.Errorf("no active FlowMusic account with usable Bearer token")
}

func cloneExcluded(excluded map[int64]struct{}) map[int64]struct{} {
	out := make(map[int64]struct{}, len(excluded))
	for id := range excluded {
		out[id] = struct{}{}
	}
	return out
}

func (s *AccountService) activeLeases(now time.Time) map[int64]struct{} {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	out := map[int64]struct{}{}
	for id, expiresAt := range s.leases {
		if !expiresAt.After(now) {
			delete(s.leases, id)
			continue
		}
		out[id] = struct{}{}
	}
	return out
}

func (s *AccountService) isLeased(id int64, now time.Time) bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	expiresAt, ok := s.leases[id]
	if !ok {
		return false
	}
	if !expiresAt.After(now) {
		delete(s.leases, id)
		return false
	}
	return true
}

func (s *AccountService) tryLease(id int64, ttl time.Duration) bool {
	now := time.Now().UTC()
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if s.leases == nil {
		s.leases = map[int64]time.Time{}
	}
	if expiresAt, ok := s.leases[id]; ok && expiresAt.After(now) {
		return false
	}
	s.leases[id] = now.Add(ttl)
	return true
}

func (s *AccountService) releaseLease(id int64) {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	delete(s.leases, id)
}

func (s *AccountService) lockAccountRefresh(id int64) func() {
	s.refreshMu.Lock()
	if s.refreshLocks == nil {
		s.refreshLocks = map[int64]*sync.Mutex{}
	}
	lock := s.refreshLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		s.refreshLocks[id] = lock
	}
	s.refreshMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func waitForLeaseRetry(ctx context.Context) error {
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *AccountService) tokenRefreshPolicy(ctx context.Context) (bool, time.Duration) {
	lead := s.cfg.TokenRefreshLead
	cfg, err := s.db.GetTokenRefreshConfig(ctx)
	if err != nil || cfg == nil {
		return true, lead
	}
	if cfg.RefreshBeforeExpiryMs > 0 {
		lead = time.Duration(cfg.RefreshBeforeExpiryMs) * time.Second
	}
	return cfg.Enabled && cfg.ATAutoRefreshEnabled, lead
}

func (s *AccountService) orderAccountsForCallMode(ctx context.Context, accounts []domain.Account) []domain.Account {
	cfg, err := s.db.GetCallLogicConfig(ctx)
	if err != nil || cfg == nil || strings.EqualFold(strings.TrimSpace(cfg.CallMode), "polling") {
		return accounts
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		left := accounts[i]
		right := accounts[j]
		if left.ConsecutiveErrorCount != right.ConsecutiveErrorCount {
			return left.ConsecutiveErrorCount < right.ConsecutiveErrorCount
		}
		if left.ErrorCount != right.ErrorCount {
			return left.ErrorCount < right.ErrorCount
		}
		leftUsed := accountUsedAt(left)
		rightUsed := accountUsedAt(right)
		if !leftUsed.Equal(rightUsed) {
			return leftUsed.Before(rightUsed)
		}
		return left.ID < right.ID
	})
	return accounts
}

func accountUsedAt(account domain.Account) time.Time {
	if account.LastUsedAt == nil {
		return time.Time{}
	}
	return account.LastUsedAt.UTC()
}

func (s *AccountService) applyDefaultProxy(ctx context.Context, account *domain.Account) {
	if account == nil || strings.TrimSpace(account.ProxyURL) != "" {
		return
	}
	if cfg, err := s.db.GetProxyConfig(ctx); err == nil && cfg != nil && cfg.ProxyEnabled {
		account.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
		return
	}
	account.ProxyURL = strings.TrimSpace(s.cfg.DefaultProxyURL)
}

func (s *AccountService) refreshAccountByProtocol(ctx context.Context, account domain.Account) (domain.Account, error) {
	mode := strings.ToLower(strings.TrimSpace(account.ProtocolMode))
	switch mode {
	case "protocol", "cookie", "cookies":
		if strings.TrimSpace(account.Cookies) == "" {
			return account, fmt.Errorf("cookie protocol account has no google_cookies")
		}
		return s.client.RefreshFromCookies(ctx, account)
	case "bearer", "at", "access_token", "flow_bearer":
		return s.refreshAccountByAvailableCredentials(ctx, account)
	}
	return s.refreshAccountByAvailableCredentials(ctx, account)
}

func (s *AccountService) refreshAccountByAvailableCredentials(ctx context.Context, account domain.Account) (domain.Account, error) {
	if strings.TrimSpace(account.RefreshToken) != "" {
		refreshed, err := s.client.RefreshSupabase(ctx, account)
		if err == nil {
			return refreshed, nil
		}
		account = refreshed
		if strings.TrimSpace(account.ProviderToken) == "" && strings.TrimSpace(account.ProviderRefreshToken) == "" {
			return account, err
		}
	}
	if strings.TrimSpace(account.ProviderToken) != "" || strings.TrimSpace(account.ProviderRefreshToken) != "" {
		return s.refreshWithProviderCredentials(ctx, account)
	}
	if strings.TrimSpace(account.Cookies) != "" {
		return s.client.RefreshFromCookies(ctx, account)
	}
	return account, fmt.Errorf("account has no refresh_token, provider_token, or cookie protocol credentials")
}

func (s *AccountService) refreshWithProviderCredentials(ctx context.Context, account domain.Account) (domain.Account, error) {
	providerWasRefreshed := false
	if strings.TrimSpace(account.ProviderToken) == "" {
		refreshed, err := s.client.RefreshGoogleProviderToken(ctx, account)
		if err != nil {
			return account, fmt.Errorf("provider_refresh_token Google OAuth refresh failed: %w", err)
		}
		account = refreshed
		providerWasRefreshed = true
	}
	flowBearer, err := s.client.SaveGoogle(ctx, account)
	if err != nil && !providerWasRefreshed && strings.TrimSpace(account.ProviderRefreshToken) != "" && isAuthFailure(err) {
		refreshed, refreshErr := s.client.RefreshGoogleProviderToken(ctx, account)
		if refreshErr != nil {
			return account, fmt.Errorf("provider_token FlowMusic bearer update failed: %w; provider_refresh_token Google OAuth refresh failed: %v", err, refreshErr)
		}
		account = refreshed
		providerWasRefreshed = true
		flowBearer, err = s.client.SaveGoogle(ctx, account)
	}
	if err != nil {
		return account, fmt.Errorf("provider_token FlowMusic bearer update failed: %w", err)
	}
	if strings.TrimSpace(flowBearer) == "" {
		return account, fmt.Errorf("provider_token FlowMusic bearer update returned empty access_token")
	}
	account.FlowBearer = flowBearer
	account.AT = flowBearer
	account.AccessToken = flowBearer
	now := time.Now().UTC()
	account.LastRefreshAt = &now
	if providerWasRefreshed {
		account.LastRefreshResult = "provider_refresh_token_google_refresh_and_flow_bearer_success"
	} else {
		account.LastRefreshResult = "provider_token_flow_bearer_success"
	}
	return account, nil
}

func nullableTimeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func hasCurrentFlowBearer(account domain.Account, now time.Time) bool {
	if strings.TrimSpace(firstNonEmpty(account.FlowBearer, account.AT)) == "" {
		return false
	}
	return account.ATExpires == nil || account.ATExpires.After(now)
}

func canRefreshAccount(account domain.Account) bool {
	mode := strings.ToLower(strings.TrimSpace(account.ProtocolMode))
	switch mode {
	case "protocol", "cookie", "cookies":
		return strings.TrimSpace(account.Cookies) != ""
	case "bearer", "at", "access_token", "flow_bearer":
		return hasRefreshCredentials(account)
	default:
		return hasRefreshCredentials(account)
	}
}

func hasRefreshCredentials(account domain.Account) bool {
	return strings.TrimSpace(account.RefreshToken) != "" ||
		strings.TrimSpace(account.ProviderToken) != "" ||
		strings.TrimSpace(account.ProviderRefreshToken) != "" ||
		strings.TrimSpace(account.Cookies) != ""
}

func shouldRefreshAccount(account domain.Account, lead time.Duration, now time.Time) bool {
	if strings.TrimSpace(firstNonEmpty(account.FlowBearer, account.AT)) == "" {
		return true
	}
	if account.ATExpires != nil {
		if account.ATExpires.Sub(now) <= lead {
			return true
		}
		if account.LastRefreshAt != nil {
			interval := time.Duration(account.RefreshIntervalMins) * time.Minute
			if interval <= 0 {
				interval = 60 * time.Minute
			}
			return now.Sub(*account.LastRefreshAt) >= interval
		}
		return false
	}
	interval := time.Duration(account.RefreshIntervalMins) * time.Minute
	if interval <= 0 {
		interval = 60 * time.Minute
	}
	return account.LastRefreshAt == nil || now.Sub(*account.LastRefreshAt) >= interval
}
