package statefile

import (
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/gen"
	"github.com/AMA-Link/armadra-cloud/go/internal/protocol/schema"
)

// stateSchema 是 state.json 的校验规则，逐条对应 apps/relay/src/personal/state.ts 的 zod schema。

func str() *schema.Schema { return &schema.Schema{Type: "string"} }

func pattern(p string) *schema.Schema { return &schema.Schema{Type: "string", Pattern: p} }

func integer() *schema.Schema { return &schema.Schema{Type: "integer"} }

func nonNegative() *schema.Schema {
	return &schema.Schema{Type: "integer", Minimum: schema.Num(0)}
}

func positive() *schema.Schema {
	return &schema.Schema{Type: "integer", ExclusiveMinimum: schema.Num(0)}
}

func nullable(s *schema.Schema) *schema.Schema {
	return &schema.Schema{AnyOf: []*schema.Schema{s, {Type: "null"}}}
}

func enum(values ...any) *schema.Schema { return &schema.Schema{Type: "string", Enum: values} }

func object(required []string, props ...schema.Property) *schema.Schema {
	return &schema.Schema{Type: "object", Properties: props, Required: required}
}

func prop(name string, s *schema.Schema) schema.Property {
	return schema.Property{Name: name, Schema: s}
}

func array(items *schema.Schema) *schema.Schema { return &schema.Schema{Type: "array", Items: items} }

const (
	hex32 = "^[0-9a-f]{32}$"
	hex64 = "^[0-9a-f]{64}$"
)

var scopeID = pattern("^[A-Za-z0-9_.:-]{1,128}$")

var rateLimitValue = &schema.Schema{Type: "integer", Minimum: schema.Num(1), Maximum: schema.Num(1_000_000)}

var stateSchema = object(
	[]string{"version", "revision", "issuer", "account", "signingKeys", "sources", "registrationTokens", "devices", "links", "settings"},
	prop("version", &schema.Schema{Type: "integer", Const: float64(Version), HasConst: true}),
	prop("revision", nonNegative()),
	prop("issuer", &schema.Schema{Type: "string", MinLength: schema.Int(1)}),
	prop("account", object(
		[]string{"name", "password", "createdAtMs", "updatedAtMs", "failures", "lockedUntilMs", "lastFailureAtMs"},
		prop("name", &schema.Schema{Type: "string", MinLength: schema.Int(1), MaxLength: schema.Int(64)}),
		prop("password", object(
			[]string{"kdf", "cost", "block", "parallel", "length", "salt", "hash"},
			prop("kdf", &schema.Schema{Type: "string", Const: "scrypt", HasConst: true}),
			prop("cost", positive()), prop("block", positive()), prop("parallel", positive()), prop("length", positive()),
			prop("salt", &schema.Schema{Type: "string", MinLength: schema.Int(1)}),
			prop("hash", &schema.Schema{Type: "string", MinLength: schema.Int(1)}),
		)),
		prop("createdAtMs", integer()), prop("updatedAtMs", integer()),
		prop("failures", nonNegative()), prop("lockedUntilMs", nonNegative()), prop("lastFailureAtMs", nonNegative()),
	)),
	prop("signingKeys", &schema.Schema{
		Type: "array",
		Items: object(
			[]string{"kid", "privateKeyPkcs8", "publicJwk", "activeFromMs", "retiredAtMs"},
			prop("kid", &schema.Schema{Type: "string", MinLength: schema.Int(1)}),
			prop("privateKeyPkcs8", &schema.Schema{Type: "string", MinLength: schema.Int(1)}),
			prop("publicJwk", gen.Ed25519PublicJWKSchema),
			prop("activeFromMs", integer()),
			prop("retiredAtMs", nullable(integer())),
		),
	}),
	prop("sources", array(object(
		[]string{"sourceId", "name", "kind", "publicJwk", "coreVersion", "capabilities", "registeredAtMs", "lastSeenAtMs", "revokedAtMs", "trustedOrigins"},
		prop("sourceId", pattern(hex32)),
		prop("name", str()),
		prop("kind", enum("desktop", "server", "hosted")),
		prop("publicJwk", gen.Ed25519PublicJWKSchema),
		prop("coreVersion", str()),
		prop("capabilities", array(str())),
		prop("registeredAtMs", integer()),
		prop("lastSeenAtMs", nullable(integer())),
		prop("revokedAtMs", nullable(integer())),
		prop("trustedOrigins", array(str())),
	))),
	prop("registrationTokens", array(object(
		[]string{"tokenHash", "expiresAtMs"},
		prop("tokenHash", pattern(hex64)),
		prop("expiresAtMs", integer()),
	))),
	prop("devices", array(object(
		[]string{"deviceId", "subject", "platform", "name", "refreshHash", "previousRefreshHash", "rotation", "createdAtMs", "expiresAtMs", "revokedAtMs", "lastSeenAtMs"},
		prop("deviceId", pattern(hex32)),
		prop("subject", &schema.Schema{Type: "string", MinLength: schema.Int(1)}),
		prop("platform", enum("ios", "android", "desktop", "browser", "server")),
		prop("name", str()),
		prop("refreshHash", pattern(hex64)),
		prop("previousRefreshHash", nullable(pattern(hex64))),
		prop("rotation", positive()),
		prop("createdAtMs", integer()), prop("expiresAtMs", integer()),
		prop("revokedAtMs", nullable(integer())), prop("lastSeenAtMs", nullable(integer())),
	))),
	prop("links", array(object(
		[]string{"linkId", "sourceId", "invitationId", "label", "role", "secretHash", "maxUses", "uses", "createdAtMs", "expiresAtMs", "revokedAtMs"},
		prop("linkId", pattern("^[0-9a-f]{16}$")),
		prop("sourceId", pattern(hex32)),
		prop("invitationId", &schema.Schema{Type: "string", MinLength: schema.Int(1)}),
		prop("label", str()), prop("role", str()),
		prop("secretHash", pattern(hex64)),
		prop("maxUses", nullable(positive())),
		prop("uses", nonNegative()),
		prop("createdAtMs", integer()), prop("expiresAtMs", integer()),
		prop("revokedAtMs", nullable(integer())),
		prop("scope", object(nil,
			prop("workspaceId", scopeID), prop("sessionId", scopeID), prop("readOnly", &schema.Schema{Type: "boolean"}))),
	))),
	prop("settings", object(
		[]string{"allowGuests", "trustedOrigins"},
		prop("allowGuests", &schema.Schema{Type: "boolean"}),
		prop("trustedOrigins", array(str())),
		prop("rateLimits", object(nil,
			prop("anonymous", rateLimitValue), prop("preflight", rateLimitValue),
			prop("token", rateLimitValue), prop("account", rateLimitValue))),
		prop("displayName", str()),
		prop("trustedProxy", object(
			[]string{"proxies"},
			prop("proxies", &schema.Schema{Type: "array", MaxItems: schema.Int(64), Items: str()}),
			prop("prefer", enum("forwarded-for", "real-ip")),
		)),
	)),
)
