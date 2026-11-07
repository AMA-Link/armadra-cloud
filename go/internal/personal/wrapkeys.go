package personal

import (
	"context"
	"fmt"

	"github.com/AMA-Link/armadra-cloud/go/internal/store/sqlite"
)

// 签名私钥的主密钥包裹（#217）：SQLite 存储给了 --master-key-file 就把新写的钥包裹后落库；
// wrap-keys 把库里已有的明文私钥就地包裹。没有反向解包，回退靠备份。

// KeyWrapStats 数存储里没包裹与已包裹的签名私钥；存储不支持（PostgreSQL）答 ok=false。
func KeyWrapStats(ctx context.Context, st Store) (plain, wrapped int, ok bool, err error) {
	s, isDB := st.(*dbStore)
	if !isDB {
		return 0, 0, false, nil
	}
	d, isSQLite := s.b.(*sqlite.DB)
	if !isSQLite {
		return 0, 0, false, nil
	}
	plain, wrapped, err = d.KeyWrapStats(ctx)
	return plain, wrapped, true, s.wrapErr(err)
}

// WrapKeysOptions 是 personal wrap-keys 的参数。
type WrapKeysOptions struct {
	Common
}

// ParseWrapKeys 解 personal wrap-keys 的参数：只收数据目录与 --master-key-file，主密钥必须给。
func ParseWrapKeys(argv []string, env Env, cwd string) (WrapKeysOptions, error) {
	values, _, err := ParseFlags(argv, withDatabase("data-dir"), FlagOptions{})
	if err != nil {
		return WrapKeysOptions{}, err
	}
	common, err := commonOf(values, env, cwd)
	if err != nil {
		return WrapKeysOptions{}, err
	}
	if common.Database.masterKey() == nil {
		return WrapKeysOptions{}, Usagef("缺少 --master-key-file（或 RELAY_MASTER_KEY_FILE）：包裹要用主密钥")
	}
	return WrapKeysOptions{Common: common}, nil
}

// RunWrapKeys 是 `personal wrap-keys`：把库里没包裹的签名私钥用主密钥就地包裹，可重复执行。
// serve 在运行时拒绝（先停 serve 再包裹，之后 serve 带主密钥启动）。
func RunWrapKeys(ctx context.Context, o WrapKeysOptions, out Print) error {
	if o.Database.masterKey() == nil {
		return Usagef("缺少 --master-key-file（或 RELAY_MASTER_KEY_FILE）：包裹要用主密钥")
	}
	kind, _, err := ResolveKind(o.DataDir, "")
	if err != nil {
		return asStateError(err)
	}
	if kind != StoreSQLite {
		return Usagef("wrap-keys 只用于 SQLite 存储；PostgreSQL 的签名钥由带主密钥的新写入包裹")
	}
	if err := refuseWhileServing(o.DataDir); err != nil {
		return err
	}
	return WithStore(ctx, o.DataDir, o.Database, func(st Store) error {
		s, ok := st.(*dbStore)
		if !ok {
			return Statef(Invalid, "存储不支持包裹签名钥")
		}
		d, ok := s.b.(*sqlite.DB)
		if !ok {
			return Statef(Invalid, "存储不支持包裹签名钥")
		}
		count, err := d.WrapSigningKeys(ctx)
		if err != nil {
			return s.wrapErr(err)
		}
		_, wrapped, _, err := KeyWrapStats(ctx, st)
		if err != nil {
			return err
		}
		if count == 0 {
			out(fmt.Sprintf("没有需要包裹的签名钥（已包裹 %d 把）", wrapped))
			return nil
		}
		out(fmt.Sprintf("已包裹 %d 把签名钥（共 %d 把已包裹）；之后 serve 要带 --master-key-file", count, wrapped))
		return nil
	})
}
