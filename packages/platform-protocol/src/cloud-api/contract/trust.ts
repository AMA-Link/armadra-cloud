import { z } from "zod";

import { errors } from "../../errors/registry.ts";
import { base, SINCE_0_10 } from "../meta.ts";
import { empty } from "../schemas.ts";
import { spaceSchema, trustRequestSchema } from "../saas-schemas.ts";

/** 主机 owner，或（团队主机的）团队 admin：背书人。 */
const endorser = { scope: ["owner", "team:admin"], since: SINCE_0_10 } as const;

/**
 * 新设备待信任（cloud-api §31，设计 §11.4）。团队成员第一次从新设备连团队主机会停在
 * `device_pending`；云端**不担保设备**，也不参与批准——批准走端到端通道发到主机（现有的
 * 「已受信设备背书」路径），云端看不到也改不了背书内容。云端只做两件事：把待信任请求
 * （设备名、平台、SAS）推给背书人（`trust.list` + me-stream 的 `deviceTrustRequested` /
 * `deviceTrustResolved`），以及记下背书人的拒绝（`trust.reject`）。仅 saas，预留。
 */
export const trust = {
  /** 缺省列调用者能背书的全部空间；给 `space` 只列那个空间。 */
  list: base
    .route({ method: "GET", path: "/v1/trust-requests" })
    .meta(endorser)
    .errors(errors.pick("team_role_insufficient", "not_found"))
    .input(z.object({ space: spaceSchema.optional() }))
    .output(z.object({ requests: z.array(trustRequestSchema) })),
  /** 拒绝：请求作废，设备继续停在 `device_pending`；已被批准或过期的答 `409 conflict`。 */
  reject: base
    .route({ method: "POST", path: "/v1/trust-requests/{requestId}/reject" })
    .meta(endorser)
    .errors(
      errors.pick(
        "not_found",
        "team_role_insufficient",
        "device_untrusted",
        "conflict",
      ),
    )
    .input(z.object({ requestId: z.string() }))
    .output(empty),
};
