package store

import (
	"context"
	"fmt"
)

func (s *Store) GetTodayGuestUsage(ctx context.Context, clientIP string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, s.bind(`SELECT use_count FROM guest_usage WHERE client_ip = ? AND date = date('now')`), clientIP).Scan(&count)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return 0, nil
		}
		return 0, fmt.Errorf("get guest usage: %w", translateErr(err))
	}
	return count, nil
}

func (s *Store) GetTodayGlobalGuestUsage(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, s.bind(`SELECT COALESCE(SUM(use_count), 0) FROM guest_usage WHERE date = date('now')`)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get global guest usage: %w", translateErr(err))
	}
	return count, nil
}

func (s *Store) IncrementGuestUsage(ctx context.Context, clientIP string) error {
	_, err := s.db.ExecContext(ctx, s.bind(`INSERT INTO guest_usage (client_ip, date, use_count) VALUES (?, date('now'), 1) ON CONFLICT(client_ip, date) DO UPDATE SET use_count = use_count + 1, updated_at = CURRENT_TIMESTAMP`), clientIP)
	return err
}
