import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import {
  invoiceSchema,
  nextCursorSchema,
  pageInputShape,
  periodSchema,
  spaceSchema,
  usageEntrySchema,
  usageSeriesPointSchema,
} from "../saas-schemas.ts";

/** 个人空间由本人（session）管，团队空间要 owner / billing。 */
const billingScope = {
  scope: ["session", "team:billing"],
  since: SINCE_0_10,
} as const;
/** 计划与用量只读：团队空间 owner / admin / billing 都能看（0.11.0 起放开 admin，写过程仍只给 owner / billing）。 */
const billingReadScope = {
  scope: ["session", "team:admin", "team:billing"],
  since: SINCE_0_10,
} as const;
const billingErrors = errors.pick("not_found", "team_role_insufficient");
const space = { space: spaceSchema };

/** 计划与订阅（1.4 起按空间；0.1 的 `orgId` 从未发出，原地换成 `space`）。 */
export const billingPlanSchema = z.object({
  space: spaceSchema,
  plan: z.string(),
  status: z.enum(["active", "trialing", "past_due", "canceled"]),
  seats: z.number().int().nonnegative(),
  periodEndsAtMs: z.number().int().nullable(),
  /** 试用到期（M2）；不在试用期时缺省。 */
  trialEndsAtMs: z.number().int().nullable().optional(),
  cancelAtPeriodEnd: z.boolean().optional(),
});

export const billingUsageSchema = z.object({
  space: spaceSchema,
  period: periodSchema,
  usage: z.array(usageEntrySchema),
  /** `granularity = "day"` 时才有：本计费周期内每天一条。 */
  series: z.array(usageSeriesPointSchema).optional(),
});

/** 回到控制台的地址，必须是控制面的 `APP_ORIGIN` 之下。 */
const returnUrl = z.url().max(2048).optional();

/**
 * 计划、用量与付费（cloud-api §26）：按空间，`space` 是 `{ kind: "personal" }` 或 `{ kind: "team", teamId }`。仅 saas，预留；
 * `plan` / `usage` 属 M1，`checkout` / `portal` / `webhook` 属 M2。
 */
export const billing = {
  plan: base
    .route({ method: "GET", path: "/v1/billing/plan" })
    .meta(billingReadScope)
    .errors(billingErrors)
    .input(z.object(space))
    .output(billingPlanSchema),
  usage: base
    .route({ method: "GET", path: "/v1/billing/usage" })
    .meta(billingReadScope)
    .errors(billingErrors)
    .input(
      z.object({
        ...space,
        period: periodSchema.optional(),
        granularity: z.enum(["month", "day"]).optional(),
      }),
    )
    .output(billingUsageSchema),
  /** 支付服务的结账页；席位随成员数自动调整。 */
  checkout: base
    .route({ method: "POST", path: "/v1/billing/checkout" })
    .meta(billingScope)
    .errors({
      ...billingErrors,
      ...errors.pick(
        "bad_request",
        "conflict",
        "unavailable",
        "plan_feature_unavailable",
      ),
    })
    .input(
      z.object({
        ...space,
        plan: z.string().min(1).max(32),
        returnUrl,
      }),
    )
    .output(z.object({ url: z.url(), expiresAtMs: z.number().int() })),
  /** 发票列表（M2），游标分页；`hostedUrl` / `pdfUrl` 是支付服务托管的页面。 */
  invoices: base
    .route({ method: "GET", path: "/v1/billing/invoices" })
    .meta(billingScope)
    .errors({ ...billingErrors, ...errors.pick("unavailable") })
    .input(z.object({ ...space, ...pageInputShape }))
    .output(
      z.object({
        invoices: z.array(invoiceSchema),
        nextCursor: nextCursorSchema,
      }),
    ),
  /** 支付服务的客户门户（付款方式、发票）。 */
  portal: base
    .route({ method: "POST", path: "/v1/billing/portal" })
    .meta(billingScope)
    .errors({ ...billingErrors, ...errors.pick("conflict", "unavailable") })
    .input(z.object({ ...space, returnUrl }))
    .output(z.object({ url: z.url() })),
  /**
   * 支付服务回调：不带账号，`scope = billing-webhook`——按提供方的签名头验证原始请求体，
   * 对不上答 `401 webhook_signature_invalid`。体按提供方的原样接收，不在契约里展开。
   */
  webhook: base
    .route({ method: "POST", path: "/v1/billing/webhooks/{provider}" })
    .meta({ scope: ["billing-webhook"], since: SINCE_0_10 })
    .errors(
      errors.pick("webhook_signature_invalid", "bad_request", "not_found"),
    )
    .input(
      z.looseObject({
        provider: z.string().regex(/^[a-z0-9][a-z0-9_-]{0,31}$/),
      }),
    )
    .output(z.object({ received: z.literal(true) })),
};
