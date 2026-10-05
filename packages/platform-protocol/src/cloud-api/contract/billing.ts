import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_1 } from "../meta.ts";

const admin = { scope: ["org:admin"], since: SINCE_0_1 } as const;

/** 仅 saas，阶段 6；只读。形状为预留，计费落地时只加字段。 */
export const billingPlanSchema = z.object({
  orgId: z.string(),
  plan: z.string(),
  status: z.enum(["active", "trialing", "past_due", "canceled"]),
  seats: z.number().int().nonnegative(),
  periodEndsAtMs: z.number().int().nullable(),
});

export const billingUsageSchema = z.object({
  orgId: z.string(),
  period: z.string().regex(/^\d{4}-\d{2}$/),
  usage: z.array(
    z.object({
      metric: z.string(),
      value: z.number().nonnegative(),
      limit: z.number().nullable(),
    }),
  ),
});

export const billing = {
  plan: base
    .route({ method: "GET", path: "/v1/orgs/{orgId}/billing" })
    .meta(admin)
    .errors(errors.pick("not_found", "org_role_insufficient"))
    .input(z.object({ orgId: z.string() }))
    .output(billingPlanSchema),
  usage: base
    .route({ method: "GET", path: "/v1/orgs/{orgId}/usage" })
    .meta(admin)
    .errors(errors.pick("not_found", "org_role_insufficient"))
    .input(
      z.object({
        orgId: z.string(),
        period: billingUsageSchema.shape.period.optional(),
      }),
    )
    .output(billingUsageSchema),
};
