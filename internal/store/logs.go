package store

import (
	"context"
	"database/sql"
	"time"

	"flowmusic2api/internal/domain"
)

func (s *Store) CreateRequestLog(ctx context.Context, entry domain.RequestLog) (int64, error) {
	query := s.bind(`INSERT INTO request_logs (account_id, operation, request_body, response_body, response_body_excerpt, status_code, duration_ms, status_text, progress, error_summary, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP) ` + returningID())
	row := s.db.QueryRowContext(ctx, query, entry.AccountID, entry.Operation, entry.RequestBody, entry.ResponseBody, entry.ResponseExcerpt, entry.StatusCode, entry.DurationMS, entry.StatusText, entry.Progress, entry.ErrorSummary)
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) UpdateRequestLog(ctx context.Context, id int64, entry domain.RequestLog) error {
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE request_logs SET account_id = ?, request_body = ?, response_body = ?, response_body_excerpt = ?, status_code = ?, duration_ms = ?, status_text = ?, progress = ?, error_summary = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), entry.AccountID, entry.RequestBody, entry.ResponseBody, entry.ResponseExcerpt, entry.StatusCode, entry.DurationMS, entry.StatusText, entry.Progress, entry.ErrorSummary, id)
	return err
}

func (s *Store) GetLogs(ctx context.Context, limit int) ([]domain.RequestLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT l.id, l.account_id, COALESCE(a.email, ''), l.operation, '' AS request_body, '' AS response_body, l.response_body_excerpt, l.status_code, l.duration_ms, l.status_text, l.progress, l.error_summary, l.created_at, l.updated_at FROM request_logs l LEFT JOIN accounts a ON a.id = l.account_id ORDER BY l.id DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLogs(rows)
}

func (s *Store) GetActiveLogs(ctx context.Context, limit int) ([]domain.RequestLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, s.bind(`SELECT l.id, l.account_id, COALESCE(a.email, ''), l.operation, '' AS request_body, '' AS response_body, l.response_body_excerpt, l.status_code, l.duration_ms, l.status_text, l.progress, l.error_summary, l.created_at, l.updated_at FROM request_logs l LEFT JOIN accounts a ON a.id = l.account_id WHERE l.status_code = ? OR LOWER(l.status_text) IN ('started', 'queued', 'token_selected', 'token_ready', 'streaming', 'polling', 'caching', 'processing', 'running') ORDER BY l.updated_at DESC, l.id DESC LIMIT ?`), 102, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanLogs(rows)
}

func (s *Store) FinalizeStaleActiveLogs(ctx context.Context, cutoff time.Time) error {
	if cutoff.IsZero() {
		return nil
	}
	_, err := s.db.ExecContext(ctx, s.bind(`UPDATE request_logs SET status_code = ?, status_text = ?, progress = ?, error_summary = CASE WHEN error_summary = '' THEN ? ELSE error_summary END, updated_at = CURRENT_TIMESTAMP WHERE (status_code = ? OR LOWER(status_text) IN ('started', 'queued', 'token_selected', 'token_ready', 'streaming', 'polling', 'caching', 'processing', 'running', 'generating')) AND updated_at < ?`),
		504,
		"timeout",
		100,
		"request did not finish before timeout; marked stale automatically",
		102,
		cutoff.UTC(),
	)
	return err
}

func (s *Store) GetLogDetail(ctx context.Context, id int64) (*domain.RequestLog, error) {
	row := s.db.QueryRowContext(ctx, s.bind(`SELECT l.id, l.account_id, COALESCE(a.email, ''), l.operation, l.request_body, l.response_body, l.response_body_excerpt, l.status_code, l.duration_ms, l.status_text, l.progress, l.error_summary, l.created_at, l.updated_at FROM request_logs l LEFT JOIN accounts a ON a.id = l.account_id WHERE l.id = ?`), id)
	log, err := scanLog(row)
	if err != nil {
		return nil, translateErr(err)
	}
	return log, nil
}

func (s *Store) DeleteOldLogs(ctx context.Context, before time.Time) error {
	if before.IsZero() {
		return nil
	}
	_, err := s.db.ExecContext(ctx, s.bind(`DELETE FROM request_logs WHERE created_at < ?`), before.UTC())
	return err
}

func (s *Store) ClearLogs(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM request_logs`)
	return err
}

func scanLog(row scanner) (*domain.RequestLog, error) {
	var log domain.RequestLog
	var accountID sql.NullInt64
	var created, updated nullableTime
	if err := row.Scan(&log.ID, &accountID, &log.AccountEmail, &log.Operation, &log.RequestBody, &log.ResponseBody, &log.ResponseExcerpt, &log.StatusCode, &log.DurationMS, &log.StatusText, &log.Progress, &log.ErrorSummary, &created, &updated); err != nil {
		return nil, err
	}
	if accountID.Valid {
		log.AccountID = &accountID.Int64
	}
	log.CreatedAt = created.Ptr()
	log.UpdatedAt = updated.Ptr()
	log.Duration = float64(log.DurationMS) / 1000
	return &log, nil
}

func scanLogs(rows *sql.Rows) ([]domain.RequestLog, error) {
	out := make([]domain.RequestLog, 0)
	for rows.Next() {
		log, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *log)
	}
	return out, rows.Err()
}
