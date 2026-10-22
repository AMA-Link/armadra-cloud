package postgres

import (
	"context"
	"encoding/json"

	"github.com/AMA-Link/armadra-cloud/go/internal/store"
)

// 行 ↔ 记录：列与记录一一对应（camelCase ↔ snake_case），*_json 列存 JSON 文本。读出来的都是新对象；
// 写都按整行写。

type scanner interface{ Scan(dest ...any) error }

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
		failures int32
	)
	if err := row.Scan(&a.AccountID, &a.Name, &password, &failures, &a.LockedUntilMs,
		&a.LastFailureAtMs, &a.CreatedAtMs, &a.UpdatedAtMs); err != nil {
		return nil, err
	}
	a.Failures = int64(failures)
	if err := decodeJSON(password, &a.Password); err != nil {
		return nil, err
	}
	return &a, nil
}

func insertAccount(ctx context.Context, q querier, tenantID string, a store.AccountRecord) error {
	_, err := q.Exec(ctx,
		`INSERT INTO accounts (tenant_id, account_id, name, password_json, failures, locked_until_ms,
		   last_failure_at_ms, created_at_ms, updated_at_ms) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		tenantID, a.AccountID, a.Name, encodeJSON(a.Password), a.Failures, a.LockedUntilMs,
		a.LastFailureAtMs, a.CreatedAtMs, a.UpdatedAtMs)
	return err
}

func writeAccount(ctx context.Context, q querier, tenantID, accountID string, a store.AccountRecord) error {
	_, err := q.Exec(ctx,
		`UPDATE accounts SET name = $1, password_json = $2, failures = $3, locked_until_ms = $4,
		   last_failure_at_ms = $5, created_at_ms = $6, updated_at_ms = $7
		 WHERE tenant_id = $8 AND account_id = $9`,
		a.Name, encodeJSON(a.Password), a.Failures, a.LockedUntilMs, a.LastFailureAtMs,
		a.CreatedAtMs, a.UpdatedAtMs, tenantID, accountID)
	return err
}

// ------------------------------------------------------------ 签名钥

// keyWrapNone 是没包裹的私钥列值。别的包裹方式本实现不认识。
const keyWrapNone = "none"

const keyColumns = "kid, private_key_pkcs8, private_key_wrap, public_jwk_json, active_from_ms, retired_at_ms"

func toSigningKey(row scanner) (store.SigningKeyRecord, error) {
	var (
		k    store.SigningKeyRecord
		wrap string
		jwk  string
	)
	if err := row.Scan(&k.Kid, &k.PrivateKeyPKCS8, &wrap, &jwk, &k.ActiveFromMs, &k.RetiredAtMs); err != nil {
		return k, err
	}
	// 只认没包裹的；别的包裹方式拒绝，不把密文当私钥用。
	if wrap != "" && wrap != keyWrapNone {
		return k, store.Errorf(store.KindUnknown, "签名钥的包裹方式不认识，拒绝启动：%s", wrap)
	}
	return k, decodeJSON(jwk, &k.PublicJWK)
}

func insertSigningKey(ctx context.Context, q querier, tenantID string, k store.SigningKeyRecord) error {
	_, err := q.Exec(ctx,
		`INSERT INTO signing_keys (tenant_id, kid, private_key_pkcs8, private_key_wrap, public_jwk_json,
		   active_from_ms, retired_at_ms) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tenantID, k.Kid, k.PrivateKeyPKCS8, keyWrapNone, encodeJSON(k.PublicJWK), k.ActiveFromMs, k.RetiredAtMs)
	return err
}

// ------------------------------------------------------------ 源

const sourceColumns = `source_id, name, kind, public_jwk_json, core_version, capabilities_json,
  registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json`

func toSource(row scanner) (*store.SourceRecord, error) {
	var (
		s                          store.SourceRecord
		jwk, capabilities, origins string
	)
	if err := row.Scan(&s.SourceID, &s.Name, &s.Kind, &jwk, &s.CoreVersion, &capabilities,
		&s.RegisteredAtMs, &s.LastSeenAtMs, &s.RevokedAtMs, &origins); err != nil {
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
	return &s, nil
}

func insertSource(ctx context.Context, q querier, tenantID string, s store.SourceRecord) error {
	_, err := q.Exec(ctx,
		`INSERT INTO sources (tenant_id, source_id, name, kind, public_jwk_json, core_version,
		   capabilities_json, registered_at_ms, last_seen_at_ms, revoked_at_ms, trusted_origins_json)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		tenantID, s.SourceID, s.Name, s.Kind, encodeJSON(s.PublicJWK), s.CoreVersion,
		encodeJSON(store.NonNil(s.Capabilities)), s.RegisteredAtMs, s.LastSeenAtMs,
		s.RevokedAtMs, encodeJSON(store.NonNil(s.TrustedOrigins)))
	return err
}

func writeSource(ctx context.Context, q querier, tenantID, sourceID string, s store.SourceRecord) error {
	_, err := q.Exec(ctx,
		`UPDATE sources SET name = $1, kind = $2, public_jwk_json = $3, core_version = $4,
		   capabilities_json = $5, registered_at_ms = $6, last_seen_at_ms = $7, revoked_at_ms = $8,
		   trusted_origins_json = $9
		 WHERE tenant_id = $10 AND source_id = $11`,
		s.Name, s.Kind, encodeJSON(s.PublicJWK), s.CoreVersion, encodeJSON(store.NonNil(s.Capabilities)),
		s.RegisteredAtMs, s.LastSeenAtMs, s.RevokedAtMs,
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

func insertRegistration(ctx context.Context, q querier, tenantID string, r store.RegistrationToken) error {
	_, err := q.Exec(ctx,
		"INSERT INTO registration_tokens (tenant_id, token_hash, account_id, expires_at_ms) VALUES ($1, $2, $3, $4)",
		tenantID, r.TokenHash, r.AccountID, r.ExpiresAtMs)
	return err
}

// ------------------------------------------------------------ 设备

const deviceColumns = `device_id, subject, platform, name, refresh_hash, previous_refresh_hash, rotation,
  created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms, transport, scope, elevated_until_ms`

const (
	transportBearer = "bearer"
	scopeSession    = "session"
)

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func toDevice(row scanner) (*store.DeviceRecord, error) {
	var (
		d                store.DeviceRecord
		rotation         int32
		transport, scope string
	)
	if err := row.Scan(&d.DeviceID, &d.Subject, &d.Platform, &d.Name, &d.RefreshHash, &d.PreviousRefreshHash,
		&rotation, &d.CreatedAtMs, &d.ExpiresAtMs, &d.RevokedAtMs, &d.LastSeenAtMs,
		&transport, &scope, &d.ElevatedUntilMs); err != nil {
		return nil, err
	}
	d.Rotation = int64(rotation)
	// 缺省值（bearer / session）在记录里是空串，个人中转的记录形状因此不变。
	if transport != transportBearer {
		d.Transport = transport
	}
	if scope != scopeSession {
		d.Scope = scope
	}
	return &d, nil
}

func insertDevice(ctx context.Context, q querier, tenantID string, d store.DeviceRecord) error {
	_, err := q.Exec(ctx,
		`INSERT INTO devices (tenant_id, device_id, subject, platform, name, refresh_hash,
		   previous_refresh_hash, rotation, created_at_ms, expires_at_ms, revoked_at_ms, last_seen_at_ms,
		   transport, scope, elevated_until_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		tenantID, d.DeviceID, d.Subject, d.Platform, d.Name, d.RefreshHash, d.PreviousRefreshHash,
		d.Rotation, d.CreatedAtMs, d.ExpiresAtMs, d.RevokedAtMs, d.LastSeenAtMs,
		orDefault(d.Transport, transportBearer), orDefault(d.Scope, scopeSession), d.ElevatedUntilMs)
	return err
}

func writeDevice(ctx context.Context, q querier, tenantID, deviceID string, d store.DeviceRecord) error {
	_, err := q.Exec(ctx,
		`UPDATE devices SET subject = $1, platform = $2, name = $3, refresh_hash = $4,
		   previous_refresh_hash = $5, rotation = $6, created_at_ms = $7, expires_at_ms = $8,
		   revoked_at_ms = $9, last_seen_at_ms = $10, transport = $11, scope = $12, elevated_until_ms = $13
		 WHERE tenant_id = $14 AND device_id = $15`,
		d.Subject, d.Platform, d.Name, d.RefreshHash, d.PreviousRefreshHash, d.Rotation,
		d.CreatedAtMs, d.ExpiresAtMs, d.RevokedAtMs, d.LastSeenAtMs,
		orDefault(d.Transport, transportBearer), orDefault(d.Scope, scopeSession), d.ElevatedUntilMs,
		tenantID, deviceID)
	return err
}

// ------------------------------------------------------------ 链接

const linkColumns = `link_id, source_id, invitation_id, label, role, secret_hash, max_uses, uses,
  created_at_ms, expires_at_ms, revoked_at_ms, scope_json`

func toLink(row scanner) (*store.LinkRecord, error) {
	var (
		l       store.LinkRecord
		maxUses *int32
		uses    int32
		scope   *string
	)
	if err := row.Scan(&l.LinkID, &l.SourceID, &l.InvitationID, &l.Label, &l.Role, &l.SecretHash,
		&maxUses, &uses, &l.CreatedAtMs, &l.ExpiresAtMs, &l.RevokedAtMs, &scope); err != nil {
		return nil, err
	}
	l.Uses = int64(uses)
	if maxUses != nil {
		v := int64(*maxUses)
		l.MaxUses = &v
	}
	if scope != nil {
		l.Scope = new(store.LinkScope)
		if err := decodeJSON(*scope, l.Scope); err != nil {
			return nil, err
		}
	}
	return &l, nil
}

func scopeJSON(l store.LinkRecord) *string {
	if l.Scope == nil {
		return nil
	}
	text := encodeJSON(l.Scope)
	return &text
}

func insertLink(ctx context.Context, q querier, tenantID string, l store.LinkRecord) error {
	_, err := q.Exec(ctx,
		`INSERT INTO links (tenant_id, link_id, source_id, invitation_id, label, role, secret_hash,
		   max_uses, uses, created_at_ms, expires_at_ms, revoked_at_ms, scope_json)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		tenantID, l.LinkID, l.SourceID, l.InvitationID, l.Label, l.Role, l.SecretHash, l.MaxUses,
		l.Uses, l.CreatedAtMs, l.ExpiresAtMs, l.RevokedAtMs, scopeJSON(l))
	return err
}

func writeLink(ctx context.Context, q querier, tenantID, linkID string, l store.LinkRecord) error {
	_, err := q.Exec(ctx,
		`UPDATE links SET source_id = $1, invitation_id = $2, label = $3, role = $4, secret_hash = $5,
		   max_uses = $6, uses = $7, created_at_ms = $8, expires_at_ms = $9, revoked_at_ms = $10, scope_json = $11
		 WHERE tenant_id = $12 AND link_id = $13`,
		l.SourceID, l.InvitationID, l.Label, l.Role, l.SecretHash, l.MaxUses, l.Uses,
		l.CreatedAtMs, l.ExpiresAtMs, l.RevokedAtMs, scopeJSON(l), tenantID, linkID)
	return err
}
