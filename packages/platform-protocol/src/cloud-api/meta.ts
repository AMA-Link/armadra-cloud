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
  /**
   * 路径或输入里带团队（`teamId`）的 procedure，要求调用者在该团队里的最低角色（cloud-api §21）：
   * `team:member` 任一角色（含 guest）；`team:admin` owner / admin；`team:owner` 仅 owner；
   * `team:billing` owner / billing。
   */
  "team:member",
  "team:admin",
  "team:owner",
  "team:billing",
  /** 分组的组长（含 owner / admin 之外的组长，cloud-api §23）。 */
  "group:lead",
  /** 运营方后台（cloud-api §28）：运营令牌，不对外公开。 */
  "operator",
  /** 支付服务回调（cloud-api §26）：不带账号，按提供方的签名头验证原始请求体。 */
  "billing-webhook",
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
export const SINCE_0_5 = "0.5.0";
/** 协议 1.4 随的协议包版本。 */
export const SINCE_0_10 = "0.10.0";
/**
 * 协议 1.4 发布后的只加不改补充，随协议包 0.11.0 发布。协议包 0.12.0 又追加了第三方登录的补充
 * （能力位 `auth.oauth.github` / `auth.oauth.google`、`auth.register` 等的错误码 `forbidden`、
 * `auth.oauthFinish` 的 `registrations_paused` 与会话分支的可选 `csrfToken`），同样只加不改、
 * 协议仍是 1.4；这些补充没有新的 procedure，所以没有对应的 `since` 常量。协议包 0.13.0、0.14.0 没有形状
 * 变化，协议仍是 1.4。
 */
export const SINCE_0_11 = "0.11.0";
/**
 * 协议包 0.15.0：只给云端控制台 API 加东西（OAuth 提权、绑定第三方登录、登录方式查询），
 * 全是加法，协议仍是 1.4（cloud-api §29）。协议包 0.15.1、0.16.0、0.17.0、0.17.1 没有形状变化，协议仍是 1.4。
 */
export const SINCE_0_15 = "0.15.0";

/** 契约构建器：每条 procedure 都必须 `.meta({ scope, since })`。 */
export const base = oc.$meta<CloudMeta>({ scope: [], since: "" });
