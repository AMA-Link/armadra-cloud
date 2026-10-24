package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 行 ↔ 记录：列与记录一一对应（camelCase ↔ snake_case），*_json 列存 JSON。读出来的都是新对象；
// 写都按整行写。

type scanner interface{ Scan(dest ...any) error }

func ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func nullable(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func decodeJSON(text string, out any) error {
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return store.Errorf(store.KindCorrupt, "库里的 JSON 列不合格式，拒绝启动")
	}
	return nil
}

func encodeJSON(v any) string { return string(store.Marshal(v)) }

// ------------------------------------------------------------ 账号

const accountColumns = "account_id, name, password_json, failures, locked_until_ms, last_failure_at_ms, created_at_ms, updated_at_ms"

func toAccount(row scanner) (*store.AccountRecord, error) {
	var (
		a        store.AccountRecord
		password string
	)
	if err := row.Scan(&a.AccountID, &a.Name, &password, &a.Failures, &a.LockedUntilMs,
		&a.LastFailureAtMs, &a.CreatedAtMs, &a.UpdatedAtMs); err != nil {
		return nil, err
	}
	if err := decodeJSON(password, &a.Password); err != nil {
		return nil, err
	}
	return &a, nil
}

func insertAccount(ctx context.Context, q *safeConn, tenantID string, a store.AccountRecord) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms,
		   last_failure_at_ms, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, a.AccountID, a.Name, encodeJSON(a.Password), a.Failures, a.LockedUntilMs,
		a.LastFailureAtMs, a.CreatedAtMs, a.UpdatedAtMs)
	return err
}

func writeAccount(ctx context.Context, q *safeConn, tenantID, accountID string, a store.AccountRecord) error {
	_, err := q.ExecContext(ctx,
		`UPDATE accounts SET name = ?, password_json = ?, failures = ?, locked_until_ms = ?,
		   last_failure_at_ms = ?, created_at_ms = ?, updated_at_ms = ?
		 WHERE tenant_id = ? AND account_id = ?`,
		a.Name, encodeJSON(a.Password), a.Failures, a.LockedUntilMs, a.LastFailureAtMs,
		a.CreatedAtMs, a.UpdatedAtMs, tenantID, accountID)
	return err
}

// ------------------------------------------------------------ 签名钥

// keyWrapNone 是没包裹的私钥列值。别的包裹方式（Workers 的 aes-256-gcm）本实现不认识。
const keyWrapNone = "none"

const keyColumns = "kid, private_key_pkcs8, private_key_wrap, public_jwk_json, active_from_ms, retired_at_ms"

func toSigningKey(row scanner) (store.SigningKeyRecord, error) {
	var (
		k       store.SigningKeyRecord
		wrap    string
		jwk     string
		retired sql.NullInt64
	)
	if err := row.Scan(&k.Kid, &k.PrivateKeyPKCS8, &wrap, &jwk, &k.ActiveFromMs, &retired); err != nil {
		return k, err
	}
	// 读签名钥：只认没包裹的；别的包裹方式拒绝，不把密文当私钥用，也不回退成明文。
	if wrap != "" && wrap != keyWrapNone {
		return k, store.Errorf(store.KindUnknown, "签名钥的包裹方式不认识，拒绝启动：%s", wrap)
	}
	if err := decodeJSON(jwk, &k.PublicJWK); err != nil {
		return k, err
	}
	k.RetiredAtMs = ptr(retired)
	return k, nil
}

func insertSigningKey(ctx context.Context, q *safeConn, tenantID string, k store.SigningKeyRecord) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO signing_keys (tenant_id, kid, private_key_pkcs8, private_key_wrap, public_jwk_json,
		   active_from_ms, retired_at_ms) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tenantID, k.Kid, k.PrivateKeyPKCS8, keyWrapNone, encodeJSON(k.PublicJWK), k.ActiveFromMs, nullable(k.RetiredAtMs))
	return err
}

// ------------------------------------------------------------ 源

const sourceColumns = `source_id, name, kind, public_jwk_json, core_version, capabilities_json,
  registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json`

func toSource(row scanner) (*store.SourceRecord, error) {
	var (
		s                          store.SourceRecord
		jwk, capabilities, origins string
		lastSeen, revoked          sql.NullInt64
	)
	if err := row.Scan(&s.SourceID, &s.Name, &s.Kind, &jwk, &s.CoreVersion, &capabilities,
		&s.RegisteredAtMs, &lastSeen, &revoked, &origins); err != nil {
		return nil, err
	}
	if err := decodeJSON(jwk, &s.PublicJWK); err != nil {
		return nil, err
	}
	if err := decodeJSON(capabilities, &s.Capabilities); err != nil {
		return nil, err
	}
	if err := decodeJSON(origins, &s.TrustedOrigins); err != nil {
		return nil, err
	}
	s.Capabilities = store.NonNil(s.Capabilities)
	s.TrustedOrigins = store.NonNil(s.TrustedOrigins)
	s.LastSeenAtMs, s.RevokedAtMs = ptr(lastSeen), ptr(revoked)
	return &s, nil
}

func insertSource(ctx context.Context, q *safeConn, tenantID string, s store.SourceRecord) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO sources (tenant_id, source_id, name, kind, public_jwk_json, core_version,
		   capabilities_json, registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, s.SourceID, s.Name, s.Kind, encodeJSON(s.PublicJWK), s.CoreVersion,
		encodeJSON(store.NonNil(s.Capabilities)), s.RegisteredAtMs, nullable(s.LastSeenAtMs),
		nullable(s.RevokedAtMs), encodeJSON(store.NonNil(s.TrustedOrigins)))
	return err
}

func writeSource(ctx context.Context, q *safeConn, tenantID, sourceID string, s store.SourceRecord) error {
	_, err := q.ExecContext(ctx,
		`UPDATE sources SET name = ?, kind = ?, public_jwk_json = ?, core_version = ?,
		   capabilities_json = ?, registered_at_ms = ?, last_seen_at_ms = ?, revoked_at_ms = ?,
		   trusted_origins_json = ?
		 WHERE tenant_id = ? AND source_id = ?`,
		s.Name, s.Kind, encodeJSON(s.PublicJWK), s.CoreVersion, encodeJSON(store.NonNil(s.Capabilities)),
		s.RegisteredAtMs, nullable(s.LastSeenAtMs), nullable(s.RevokedAtMs),
		encodeJSON(store.NonNil(s.TrustedOrigins)), tenantID, sourceID)
	return err
}

// ------------------------------------------------------------ 注册令牌

const registrationColumns = "token_hash, account_id, expires_at_ms"

func toRegistration(row scanner) (store.RegistrationToken, error) {
	var r store.RegistrationToken
	err := row.Scan(&r.TokenHash, &r.AccountID, &r.ExpiresAtMs)
	return r, err
}

func insertRegistration(ctx context.Context, q *safeConn, tenantID string, r store.RegistrationToken) error {
	_, err := q.ExecContext(ctx,
		"INSERT INTO registration_tokens (tenant_id, token_hash, account_id, expires_at_ms) VALUES (?, ?, ?, ?)",
		tenantID, r.TokenHash, r.AccountID, r.ExpiresAtMs)
	return err
}

// ------------------------------------------------------------ 设备

const deviceColumns = `device_id, subject, platform, name, refresh_hash, previous_refresh_hash, rotation,
  created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms`

func toDevice(row scanner) (*store.DeviceRecord, error) {
	var (
		d                 store.DeviceRecord
		previous          sql.NullString
		revoked, lastSeen sql.NullInt64
	)
	if err := row.Scan(&d.DeviceID, &d.Subject, &d.Platform, &d.Name, &d.RefreshHash, &previous,
		&d.Rotation, &d.CreatedAtMs, &d.ExpiresAtMs, &revoked, &lastSeen); err != nil {
		return nil, err
	}
	if previous.Valid {
		v := previous.String
		d.PreviousRefreshHash = &v
	}
	d.RevokedAtMs, d.LastSeenAtMs = ptr(revoked), ptr(lastSeen)
	return &d, nil
}

func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func insertDevice(ctx context.Context, q *safeConn, tenantID string, d store.DeviceRecord) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash,
		   previous_refresh_hash, rotation, created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, d.DeviceID, d.Subject, d.Platform, d.Name, d.RefreshHash, nullableString(d.PreviousRefreshHash),
		d.Rotation, d.CreatedAtMs, d.ExpiresAtMs, nullable(d.RevokedAtMs), nullable(d.LastSeenAtMs))
	return err
}

func writeDevice(ctx context.Context, q *safeConn, tenantID, deviceID string, d store.DeviceRecord) error {
	_, err := q.ExecContext(ctx,
		`UPDATE devices SET subject = ?, platform = ?, name = ?, refresh_hash = ?,
		   previous_refresh_hash = ?, rotation = ?, created_at_ms = ?, expires_at_ms = ?,
		   revoked_at_ms = ?, last_seen_at_ms = ?
		 WHERE tenant_id = ? AND device_id = ?`,
		d.Subject, d.Platform, d.Name, d.RefreshHash, nullableString(d.PreviousRefreshHash), d.Rotation,
		d.CreatedAtMs, d.ExpiresAtMs, nullable(d.RevokedAtMs), nullable(d.LastSeenAtMs), tenantID, deviceID)
	return err
}

// ------------------------------------------------------------ 链接

const linkColumns = `link_id, source_id, invitation_id, label, role, secret_hash, max_uses, uses,
  created_at_ms, expires_at_ms, revoked_at_ms, scope_json`

func toLink(row scanner) (*store.LinkRecord, error) {
	var (
		l                store.LinkRecord
		maxUses, revoked sql.NullInt64
		scope            sql.NullString
	)
	if err := row.Scan(&l.LinkID, &l.SourceID, &l.InvitationID, &l.Label, &l.Role, &l.SecretHash,
		&maxUses, &l.Uses, &l.CreatedAtMs, &l.ExpiresAtMs, &revoked, &scope); err != nil {
		return nil, err
	}
	l.MaxUses, l.RevokedAtMs = ptr(maxUses), ptr(revoked)
	if scope.Valid {
		l.Scope = new(store.LinkScope)
		if err := decodeJSON(scope.String, l.Scope); err != nil {
			return nil, err
		}
	}
	return &l, nil
}

func scopeJSON(l store.LinkRecord) any {
	if l.Scope == nil {
		return nil
	}
	return encodeJSON(l.Scope)
}

func insertLink(ctx context.Context, q *safeConn, tenantID string, l store.LinkRecord) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO links (tenant_id, link_id, source_id, invitation_id, label, role, secret_hash,
		   max_uses, uses, created_at_ms, expires_at_ms, revoked_at_ms, scope_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, l.LinkID, l.SourceID, l.InvitationID, l.Label, l.Role, l.SecretHash, nullable(l.MaxUses),
		l.Uses, l.CreatedAtMs, l.ExpiresAtMs, nullable(l.RevokedAtMs), scopeJSON(l))
	return err
}

func writeLink(ctx context.Context, q *safeConn, tenantID, linkID string, l store.LinkRecord) error {
	_, err := q.ExecContext(ctx,
		`UPDATE links SET source_id = ?, invitation_id = ?, label = ?, role = ?, secret_hash = ?,
		   max_uses = ?, uses = ?, created_at_ms = ?, expires_at_ms = ?, revoked_at_ms = ?, scope_json = ?
		 WHERE tenant_id = ? AND link_id = ?`,
		l.SourceID, l.InvitationID, l.Label, l.Role, l.SecretHash, nullable(l.MaxUses), l.Uses,
		l.CreatedAtMs, l.ExpiresAtMs, nullable(l.RevokedAtMs), scopeJSON(l), tenantID, linkID)
	return err
}

// ------------------------------------------------------------ 修订

// bumpRevision：租户的修订加一（每个改了东西的写事务一次）。
func bumpRevision(ctx context.Context, q *safeConn, tenantID string) error {
	_, err := q.ExecContext(ctx, "UPDATE tenants SET revision = revision + 1 WHERE tenant_id = ?", tenantID)
	return err
}
