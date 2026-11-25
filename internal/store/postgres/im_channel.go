package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/workagent/auto-agent/internal/store"
)

// GetIMChannel 取渠道注册信息。
func (d *DB) GetIMChannel(ctx context.Context, channel string) (store.IMChannel, error) {
	const q = `select channel, label, enabled from im_channel where channel = $1`

	var c store.IMChannel
	err := d.pool.QueryRow(ctx, q, channel).Scan(&c.Channel, &c.Label, &c.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.IMChannel{}, store.ErrNotFound
	}
	if err != nil {
		return store.IMChannel{}, err
	}
	return c, nil
}

// EnsureIMChannel 幂等注册渠道。
//
// 已存在时不覆盖 label / enabled：管理员把某个渠道停用之后，插件下次启动
// 不应该把它悄悄打开。渠道的实现（插件）只负责投递，不负责开关平台能力。
func (d *DB) EnsureIMChannel(ctx context.Context, channel, label string) error {
	const q = `
insert into im_channel (channel, label) values ($1, $2)
on conflict (channel) do nothing`
	_, err := d.pool.Exec(ctx, q, channel, label)
	return err
}
