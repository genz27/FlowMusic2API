package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"flowmusic2api/internal/domain"
)

const accountColumns = `a.id, a.email, a.name, a.remark, a.is_active, a.protocol_mode,
a.refresh_token, a.access_token, a.provider_token, a.provider_refresh_token, a.flow_bearer,
a.cookies, a.login_account, a.login_password, a.proxy_url, a.auto_refresh_enabled,
a.refresh_interval_minutes, a.expires_at, a.at_expires, a.last_refresh_at, a.last_refresh_result,
a.last_used_at, a.credits, a.tokens_remaining, a.subscription_tier, a.use_count,
a.music_count, a.today_music_count, a.image_count, a.video_count, a.error_count,
a.today_error_count, a.consecutive_error_count, a.created_at, a.updated_at,
a.image_enabled, a.video_enabled, a.upscale_enabled, a.image_concurrency, a.video_concurrency`

func (s *Store) ListAccounts(ctx context.Context) ([]domain.Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountColumns+` FROM accounts a ORDER BY a.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccounts(rows)
}

func (s *Store) GetActiveAccounts(ctx context.Context) ([]domain.Account, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT `+accountColumns+` FROM accounts a WHERE a.is_active = ? ORDER BY a.last_used_at ASC, a.id ASC`), true)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccounts(rows)
}

func (s *Store) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT `+accountColumns+` FROM accounts a WHERE a.id = ?`), id)
	account, err := scanAccount(row)
	if err != nil {
		return nil, translateErr(err)
	}
	return account, nil
}

func (s *Store) GetAccountByEmail(ctx context.Context, email string) (*domain.Account, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT `+accountColumns+` FROM accounts a WHERE a.email = ?`), email)
	account, err := scanAccount(row)
	if err != nil {
		return nil, translateErr(err)
	}
	return account, nil
}

func (s *Store) CreateAccount(ctx context.Context, a domain.Account) (int64, error) {
	email := strings.TrimSpace(firstNonEmpty(a.Email, a.LoginAccount))
	if email == "" {
		email = fmt.Sprintf("account-%d@local", time.Now().UnixNano())
	}
	if a.ProtocolMode == "" {
		if firstNonEmpty(a.FlowBearer, a.AT, a.AccessToken) != "" && firstNonEmpty(a.RefreshToken, a.ST) == "" {
			a.ProtocolMode = "bearer"
		} else if strings.TrimSpace(a.Cookies) != "" && firstNonEmpty(a.FlowBearer, a.AT, a.AccessToken) == "" && firstNonEmpty(a.RefreshToken, a.ST) == "" {
			a.ProtocolMode = "protocol"
		} else {
			a.ProtocolMode = "refresh_token"
		}
	}
	a.ProtocolMode = domain.NormalizeProtocolMode(a.ProtocolMode)
	if a.RefreshIntervalMins <= 0 {
		a.RefreshIntervalMins = 30
	}
	if !a.CapabilityFlagsSet && !a.ImageEnabled && !a.VideoEnabled && !a.UpscaleEnabled {
		a.ImageEnabled = true
		a.VideoEnabled = true
		a.UpscaleEnabled = true
	}
	query := s.bind(`INSERT INTO accounts (
email, name, remark, is_active, protocol_mode, refresh_token, access_token, provider_token,
provider_refresh_token, flow_bearer, cookies, login_account, login_password, proxy_url,
auto_refresh_enabled, refresh_interval_minutes, expires_at, at_expires, credits,
tokens_remaining, subscription_tier, image_enabled, video_enabled, upscale_enabled,
image_concurrency, video_concurrency, today_date, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ` + returningID())
	row := s.db.QueryRowContext(ctx, query,
		email, a.Name, a.Remark, true, a.ProtocolMode, firstNonEmpty(a.RefreshToken, a.ST),
		a.AccessToken, a.ProviderToken, a.ProviderRefreshToken, firstNonEmpty(a.FlowBearer, a.AT),
		a.Cookies, a.LoginAccount, a.LoginPassword, firstNonEmpty(a.ProxyURL, s.cfg.DefaultProxyURL),
		a.AutoRefreshEnabled, a.RefreshIntervalMins, nullableArg(a.ExpiresAt), nullableArg(a.ATExpires),
		a.Credits, a.TokensRemaining, a.SubscriptionTier, a.ImageEnabled, a.VideoEnabled, a.UpscaleEnabled,
		defaultInt(a.ImageConcurrency, -1), defaultInt(a.VideoConcurrency, -1), today())
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) UpsertAccount(ctx context.Context, a domain.Account) (int64, bool, error) {
	email := strings.TrimSpace(firstNonEmpty(a.Email, a.LoginAccount))
	if email != "" {
		existing, err := s.GetAccountByEmail(ctx, email)
		if err != nil && err != ErrNotFound {
			return 0, false, err
		}
		if existing != nil {
			a.Email = email
			if err := s.UpdateAccount(ctx, existing.ID, a); err != nil {
				return 0, false, err
			}
			return existing.ID, true, nil
		}
	}
	id, err := s.CreateAccount(ctx, a)
	return id, false, err
}

func (s *Store) UpdateAccount(ctx context.Context, id int64, a domain.Account) error {
	existing, err := s.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	email := updateStringValue(firstNonEmpty(a.Email), existing.Email, explicitAccountField(a, "email"))
	name := updateStringValue(a.Name, existing.Name, explicitAccountField(a, "name"))
	remark := updateStringValue(a.Remark, existing.Remark, explicitAccountField(a, "remark"))
	refreshToken := updateStringValue(firstNonEmpty(a.RefreshToken, a.ST), existing.RefreshToken, explicitAccountField(a, "refresh_token"))
	flowBearer := updateStringValue(firstNonEmpty(a.FlowBearer, a.AT, a.AccessToken), existing.FlowBearer, explicitAccountField(a, "flow_bearer"))
	accessToken := updateStringValue(firstNonEmpty(a.AccessToken, a.FlowBearer, a.AT), existing.AccessToken, explicitAccountField(a, "flow_bearer"))
	providerToken := updateStringValue(a.ProviderToken, existing.ProviderToken, explicitAccountField(a, "provider_token"))
	providerRefreshToken := updateStringValue(a.ProviderRefreshToken, existing.ProviderRefreshToken, explicitAccountField(a, "provider_refresh_token"))
	cookies := updateStringValue(a.Cookies, existing.Cookies, explicitAccountField(a, "cookies"))
	loginAccount := updateStringValue(a.LoginAccount, existing.LoginAccount, explicitAccountField(a, "login_account"))
	loginPassword := updateStringValue(a.LoginPassword, existing.LoginPassword, explicitAccountField(a, "login_password") && (strings.TrimSpace(a.LoginPassword) != "" || clearAccountField(a, "login_password")))
	proxyURL := updateStringValue(a.ProxyURL, existing.ProxyURL, explicitAccountField(a, "proxy_url"))
	expiresAt := existing.ExpiresAt
	if a.ExpiresAt != nil {
		expiresAt = a.ExpiresAt
	}
	atExpires := existing.ATExpires
	if a.ATExpires != nil {
		atExpires = a.ATExpires
	}
	if a.ProtocolMode == "" && !explicitAccountField(a, "protocol_mode") {
		a.ProtocolMode = existing.ProtocolMode
	}
	a.ProtocolMode = domain.NormalizeProtocolMode(a.ProtocolMode)
	if a.RefreshIntervalMins <= 0 {
		a.RefreshIntervalMins = existing.RefreshIntervalMins
	}
	if a.RefreshIntervalMins <= 0 {
		a.RefreshIntervalMins = 30
	}
	if !a.CapabilityFlagsSet && !a.ImageEnabled && !a.VideoEnabled && !a.UpscaleEnabled {
		a.ImageEnabled = existing.ImageEnabled
		a.VideoEnabled = existing.VideoEnabled
		a.UpscaleEnabled = existing.UpscaleEnabled
	}
	if a.ImageConcurrency == 0 {
		a.ImageConcurrency = existing.ImageConcurrency
	}
	if a.VideoConcurrency == 0 {
		a.VideoConcurrency = existing.VideoConcurrency
	}
	_, err = s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET
email = ?, name = ?, remark = ?, protocol_mode = ?, refresh_token = ?, access_token = ?,
provider_token = ?, provider_refresh_token = ?, flow_bearer = ?, cookies = ?, login_account = ?,
login_password = ?, proxy_url = ?, auto_refresh_enabled = ?, refresh_interval_minutes = ?,
expires_at = ?, at_expires = ?, image_enabled = ?, video_enabled = ?, upscale_enabled = ?,
image_concurrency = ?, video_concurrency = ?, updated_at = CURRENT_TIMESTAMP
WHERE id = ?`), email, name, remark, a.ProtocolMode, refreshToken,
		accessToken, providerToken, providerRefreshToken, flowBearer, cookies,
		loginAccount, loginPassword, proxyURL, a.AutoRefreshEnabled, a.RefreshIntervalMins,
		nullableArg(expiresAt), nullableArg(atExpires), a.ImageEnabled, a.VideoEnabled, a.UpscaleEnabled,
		defaultInt(a.ImageConcurrency, -1), defaultInt(a.VideoConcurrency, -1), id)
	return err
}

func explicitAccountField(account domain.Account, field string) bool {
	return account.ExplicitFields != nil && account.ExplicitFields[field]
}

func clearAccountField(account domain.Account, field string) bool {
	return account.ClearFields != nil && account.ClearFields[field]
}

func updateStringValue(next, existing string, explicit bool) string {
	next = strings.TrimSpace(next)
	if explicit {
		return next
	}
	return firstNonEmpty(next, existing)
}

func (s *Store) UpdateAccountFields(ctx context.Context, id int64, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	allowed := map[string]struct{}{
		"email": {}, "name": {}, "remark": {}, "is_active": {}, "protocol_mode": {},
		"refresh_token": {}, "access_token": {}, "provider_token": {}, "provider_refresh_token": {},
		"flow_bearer": {}, "cookies": {}, "login_account": {}, "login_password": {}, "proxy_url": {},
		"auto_refresh_enabled": {}, "refresh_interval_minutes": {}, "expires_at": {}, "at_expires": {},
		"last_refresh_at": {}, "last_refresh_result": {}, "last_used_at": {}, "credits": {},
		"tokens_remaining": {}, "subscription_tier": {}, "use_count": {}, "music_count": {},
		"today_music_count": {}, "image_count": {}, "video_count": {}, "error_count": {},
		"today_error_count": {}, "consecutive_error_count": {}, "today_date": {},
	}
	parts := make([]string, 0, len(updates)+1)
	args := make([]any, 0, len(updates)+1)
	for key, value := range updates {
		if _, ok := allowed[key]; !ok {
			continue
		}
		parts = append(parts, key+" = ?")
		args = append(args, value)
	}
	if len(parts) == 0 {
		return nil
	}
	parts = append(parts, "updated_at = CURRENT_TIMESTAMP")
	args = append(args, id)
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET `+strings.Join(parts, ", ")+` WHERE id = ?`), args...)
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.bind(`DELETE FROM accounts WHERE id = ?`), id)
	return err
}

func (s *Store) SetAccountActive(ctx context.Context, id int64, active bool) error {
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET is_active = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), active, id)
	return err
}

func (s *Store) TouchAccountUsed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET last_used_at = CURRENT_TIMESTAMP, use_count = use_count + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
	return err
}

func (s *Store) IncrementAccountStat(ctx context.Context, id int64, stat string, success bool) error {
	setDate := today()
	_, _ = s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET today_music_count = CASE WHEN today_date <> ? THEN 0 ELSE today_music_count END, today_error_count = CASE WHEN today_date <> ? THEN 0 ELSE today_error_count END, today_date = ? WHERE id = ?`), setDate, setDate, setDate, id)
	if !success {
		_, err := s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET error_count = error_count + 1, today_error_count = today_error_count + 1, consecutive_error_count = consecutive_error_count + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
		return err
	}
	switch strings.ToLower(strings.TrimSpace(stat)) {
	case "image":
		_, err := s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET image_count = image_count + 1, consecutive_error_count = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
		return err
	case "video":
		_, err := s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET video_count = video_count + 1, consecutive_error_count = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
		return err
	default:
		_, err := s.db.ExecContext(ctx, s.bind(`UPDATE accounts SET music_count = music_count + 1, today_music_count = today_music_count + 1, consecutive_error_count = 0, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
		return err
	}
}

func (s *Store) GetDashboardStats(ctx context.Context) (domain.DashboardStats, error) {
	var stats domain.DashboardStats
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN is_active THEN 1 ELSE 0 END), 0), COALESCE(SUM(music_count), 0), COALESCE(SUM(today_music_count), 0), COALESCE(SUM(image_count), 0), COALESCE(SUM(video_count), 0), COALESCE(SUM(error_count), 0), COALESCE(SUM(today_error_count), 0) FROM accounts`)
	if err := row.Scan(&stats.TotalTokens, &stats.ActiveTokens, &stats.TotalMusic, &stats.TodayMusic, &stats.TotalImages, &stats.TotalVideos, &stats.TotalErrors, &stats.TodayErrors); err != nil {
		return stats, err
	}
	stats.TodayImages = 0
	stats.TodayVideos = 0
	return stats, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanAccount(row scanner) (*domain.Account, error) {
	var a domain.Account
	var expires, atExpires, lastRefresh, lastUsed, created, updated nullableTime
	if err := row.Scan(&a.ID, &a.Email, &a.Name, &a.Remark, &a.IsActive, &a.ProtocolMode,
		&a.RefreshToken, &a.AccessToken, &a.ProviderToken, &a.ProviderRefreshToken, &a.FlowBearer,
		&a.Cookies, &a.LoginAccount, &a.LoginPassword, &a.ProxyURL, &a.AutoRefreshEnabled,
		&a.RefreshIntervalMins, &expires, &atExpires, &lastRefresh, &a.LastRefreshResult,
		&lastUsed, &a.Credits, &a.TokensRemaining, &a.SubscriptionTier, &a.UseCount,
		&a.MusicCount, &a.TodayMusicCount, &a.ImageCount, &a.VideoCount, &a.ErrorCount,
		&a.TodayErrorCount, &a.ConsecutiveErrorCount, &created, &updated, &a.ImageEnabled,
		&a.VideoEnabled, &a.UpscaleEnabled, &a.ImageConcurrency, &a.VideoConcurrency); err != nil {
		return nil, err
	}
	a.ExpiresAt = expires.Ptr()
	a.ATExpires = atExpires.Ptr()
	a.LastRefreshAt = lastRefresh.Ptr()
	a.LastUsedAt = lastUsed.Ptr()
	a.CreatedAt = created.Ptr()
	a.UpdatedAt = updated.Ptr()
	a.ST = a.RefreshToken
	a.AT = a.FlowBearer
	return &a, nil
}

func scanAccounts(rows *sql.Rows) ([]domain.Account, error) {
	out := make([]domain.Account, 0)
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *account)
	}
	return out, rows.Err()
}
