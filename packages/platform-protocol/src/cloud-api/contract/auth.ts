import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1 } from "../meta.ts";
import {
  deviceInputSchema,
  empty,
  mfaChallengeResultSchema,
  sessionResultSchema,
  sessionSchema,
} from "../schemas.ts";

const anonymous = { scope: ["anonymous"], since: SINCE_0_1 } as const;
const session = { scope: ["session"], since: SINCE_0_1 } as const;

const passwordErrors = errors.pick(
  "password_too_short",
  "password_too_long",
  "password_too_common",
  "password_contains_name",
  "password_breached",
);

/** 与 core 契约 §18.2 同形：`options` 是 WebAuthn 的 `…OptionsJSON`，`response` 是 `PublicKeyCredential.toJSON()`。 */
const webauthnJson = z.record(z.string(), z.unknown());

export const passkeySchema = z.object({
  credentialId: z.string(),
  label: z.string(),
  aaguid: z.string(),
  transports: z.array(z.string()),
  createdAtMs: z.number().int(),
});

export const deviceStartOutputSchema = z.object({
  deviceCode: z.string(),
  userCode: z.string().regex(/^[A-Z]{4}-[A-Z]{4}$/),
  verificationUrl: z.url(),
  expiresAtMs: z.number().int(),
  intervalMs: z.number().int().positive(),
});

export const devicePollOutputSchema = z.discriminatedUnion("status", [
  z.object({ status: z.literal("pending") }),
  z.object({ status: z.literal("approved"), session: sessionSchema }),
  z.object({ status: z.literal("denied") }),
  z.object({ status: z.literal("expired") }),
]);

export const auth = {
  register: base
    .route({ method: "POST", path: "/v1/auth/register" })
    .meta(anonymous)
    .errors({
      ...errors.pick("email_taken", "rate_limited"),
      ...passwordErrors,
    })
    .input(
      z.object({
        email: z.email(),
        password: z.string(),
        displayName: z.string().min(1).max(128),
        device: deviceInputSchema,
      }),
    )
    .output(sessionResultSchema),
  login: base
    .route({ method: "POST", path: "/v1/auth/login" })
    .meta(anonymous)
    .errors(
      errors.pick(
        "credentials_invalid",
        "account_locked",
        "account_disabled",
        "rate_limited",
      ),
    )
    .input(
      z.object({
        /** 邮箱，或 personal 模式的账号名。 */
        account: z.string().min(1),
        password: z.string(),
        device: deviceInputSchema,
      }),
    )
    .output(z.union([sessionResultSchema, mfaChallengeResultSchema])),
  mfaVerify: base
    .route({ method: "POST", path: "/v1/auth/mfa/verify" })
    .meta(anonymous)
    .errors(errors.pick("mfa_invalid", "mfa_expired", "account_locked"))
    .input(z.object({ challengeId: z.string(), code: z.string() }))
    .output(sessionResultSchema),
  refresh: base
    .route({ method: "POST", path: "/v1/auth/refresh" })
    .meta(anonymous)
    .errors(errors.pick("session_expired", "session_revoked"))
    .input(z.object({ refreshToken: z.string() }))
    .output(sessionResultSchema),
  logout: base
    .route({ method: "POST", path: "/v1/auth/logout" })
    .meta(session)
    .input(empty)
    .output(empty),
  deviceStart: base
    .route({ method: "POST", path: "/v1/auth/device/start" })
    .meta(anonymous)
    .errors(errors.pick("rate_limited"))
    .input(z.object({ device: deviceInputSchema }))
    .output(deviceStartOutputSchema),
  devicePoll: base
    .route({ method: "POST", path: "/v1/auth/device/poll" })
    .meta(anonymous)
    .errors(errors.pick("rate_limited"))
    .input(z.object({ deviceCode: z.string() }))
    .output(devicePollOutputSchema),
  deviceApprove: base
    .route({ method: "POST", path: "/v1/auth/device/approve" })
    .meta(session)
    .errors(errors.pick("device_code_expired", "not_found"))
    .input(z.object({ userCode: z.string(), approve: z.boolean() }))
    .output(empty),
  passkeyRegisterOptions: base
    .route({ method: "POST", path: "/v1/auth/passkey/register/options" })
    .meta(session)
    .input(z.object({ label: z.string().max(64).optional() }))
    .output(z.object({ challengeId: z.string(), options: webauthnJson })),
  passkeyRegister: base
    .route({ method: "POST", path: "/v1/auth/passkey/register" })
    .meta(session)
    .input(
      z.object({
        challengeId: z.string(),
        response: webauthnJson,
        label: z.string().max(64).optional(),
      }),
    )
    .output(passkeySchema),
  passkeyLoginOptions: base
    .route({ method: "POST", path: "/v1/auth/passkey/login/options" })
    .meta(anonymous)
    .errors(errors.pick("rate_limited"))
    .input(empty)
    .output(z.object({ challengeId: z.string(), options: webauthnJson })),
  passkeyLogin: base
    .route({ method: "POST", path: "/v1/auth/passkey/login" })
    .meta(anonymous)
    .errors(errors.pick("credentials_invalid", "rate_limited"))
    .input(
      z.object({
        challengeId: z.string(),
        response: webauthnJson,
        device: deviceInputSchema,
      }),
    )
    .output(sessionResultSchema),
  totpEnroll: base
    .route({ method: "POST", path: "/v1/auth/totp/enroll" })
    .meta(session)
    .errors(errors.pick("conflict"))
    .input(empty)
    .output(z.object({ secret: z.string(), otpauthUri: z.string() })),
  totpConfirm: base
    .route({ method: "POST", path: "/v1/auth/totp/confirm" })
    .meta(session)
    .errors(errors.pick("mfa_invalid"))
    .input(z.object({ code: z.string() }))
    .output(z.object({ recoveryCodes: z.array(z.string()) })),
  totpDisable: base
    .route({ method: "POST", path: "/v1/auth/totp/disable" })
    .meta(session)
    .errors(errors.pick("mfa_invalid"))
    .input(z.object({ code: z.string() }))
    .output(z.object({ disabled: z.literal(true) })),
  recoveryCodesRotate: base
    .route({ method: "POST", path: "/v1/auth/recovery-codes/rotate" })
    .meta(session)
    .errors(errors.pick("mfa_invalid"))
    .input(z.object({ code: z.string() }))
    .output(z.object({ recoveryCodes: z.array(z.string()) })),
};
