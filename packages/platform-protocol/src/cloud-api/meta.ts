/**
 * 每条 procedure 的 meta：谁能调（`scope`）与从哪个协议包版本起有（`since`）。
 * `@orpc/*` 只能出现在 `src/cloud-api/` 里（AGENTS.md）。
 */
import { oc } from "@orpc/contract";

export const SCOPES = [
  "anonymous",
  "session",
  "owner",
  "source",
  "org:admin",
  "org:owner",
  /** 中继节点对控制面（cloud-api §17）：`Authorization: Bearer <节点令牌>`。 */
  "relay-node",
] as const;
export type Scope = (typeof SCOPES)[number];

export interface CloudMeta {
  /** 满足其一即可；`links.create` 这类按输入分情况的列多个。 */
  scope: readonly Scope[];
  /** 协议包版本。 */
  since: string;
}

export const SINCE_0_1 = "0.1.0";
export const SINCE_0_2 = "0.2.0";
export const SINCE_0_3_5 = "0.3.5";
export const SINCE_0_3_10 = "0.3.10";
export const SINCE_0_4 = "0.4.0";

/** 契约构建器：每条 procedure 都必须 `.meta({ scope, since })`。 */
export const base = oc.$meta<CloudMeta>({ scope: [], since: "" });
