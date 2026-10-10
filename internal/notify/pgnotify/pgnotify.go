// Package pgnotify 是 notify.Registry 的 PostgreSQL 实现。
package pgnotify

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"yanshi/internal/notify"
)

type Registry struct{ Pool *pgxpool.Pool }

var _ notify.Registry = Registry{}

func (r Registry) Register(ctx context.Context, d *notify.Device, at time.Time) error {
	_, err := r.Pool.Exec(ctx, `
		INSERT INTO push_devices (business_line, end_user, device_id, label, kind, platform, token, last_active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (business_line, end_user, device_id) DO UPDATE SET label = $4, kind = $5, platform = $6, token = $7`,
		d.BusinessLine, d.EndUser, d.DeviceID, d.Label, d.Kind, d.Platform, d.Token, at)
	return err
}

func (r Registry) Touch(ctx context.Context, bl, eu, dev string, at time.Time) error {
	_, err := r.Pool.Exec(ctx, `
		UPDATE push_devices SET last_active = $4
		WHERE business_line = $1 AND end_user = $2 AND device_id = $3 AND last_active < $4`, bl, eu, dev, at)
	return err
}

func (r Registry) List(ctx context.Context, bl, eu string) ([]*notify.Device, error) {
	rows, err := r.Pool.Query(ctx, `
		SELECT device_id, label, kind, platform, token, last_active FROM push_devices
		WHERE business_line = $1 AND end_user = $2 ORDER BY last_active DESC, device_id`, bl, eu)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (*notify.Device, error) {
		d := &notify.Device{BusinessLine: bl, EndUser: eu}
		return d, row.Scan(&d.DeviceID, &d.Label, &d.Kind, &d.Platform, &d.Token, &d.LastActive)
	})
}

func (r Registry) Unregister(ctx context.Context, bl, eu, dev string) error {
	_, err := r.Pool.Exec(ctx, `DELETE FROM push_devices WHERE business_line = $1 AND end_user = $2 AND device_id = $3`, bl, eu, dev)
	return err
}

func (r Registry) DeleteEndUser(ctx context.Context, bl, eu string) error {
	_, err := r.Pool.Exec(ctx, `DELETE FROM push_devices WHERE business_line = $1 AND end_user = $2`, bl, eu)
	return err
}
