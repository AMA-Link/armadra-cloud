/**
 * `/v1/*` 契约（协议包设计 §5）。cloud 用 `implement(cloudContract)`；页面 / 手机 / core
 * 用 `createORPCClient(new OpenAPILink(cloudContract, { url }))`。
 */
import {
  isContractProcedure,
  type AnyContractProcedure,
  type ErrorMap,
} from "@orpc/contract";

import { admin, status } from "./contract/admin.ts";
import { apiTokens } from "./contract/apiTokens.ts";
import { audit } from "./contract/audit.ts";
import { auth } from "./contract/auth.ts";
import { billing } from "./contract/billing.ts";
import { groups } from "./contract/groups.ts";
import { accessRequests, grants } from "./contract/grants.ts";
import { invitations } from "./contract/invitations.ts";
import { links } from "./contract/links.ts";
import { me } from "./contract/me.ts";
import { platform } from "./contract/platform.ts";
import { relay } from "./contract/relay.ts";
import { sources } from "./contract/sources.ts";
import { teams } from "./contract/teams.ts";
import { trust } from "./contract/trust.ts";
import type { CloudMeta } from "./meta.ts";

export const cloudContract = {
  platform,
  auth,
  me,
  sources,
  links,
  teams,
  invitations,
  groups,
  grants,
  accessRequests,
  trust,
  audit,
  billing,
  apiTokens,
  admin,
  status,
  relay,
};
export type CloudContract = typeof cloudContract;

/**
 * `docs/contracts/cloud-api.md` 的节号：按顶层域，`me.streamTicket` 归 §6、`auth.changePassword` 归 §18、
 * `me.traffic` 归 §19；1.4 起 SaaS 的各域归 §21–§28（`trust` 归 §31），账号补充（邮箱验证、找回口令、第三方登录、
 * 改邮箱、空间、安全日志、注销、导出）归 §29。§3（组织）与 §9（计费，原挂组织）已移除。
 */
export const CONTRACT_SECTIONS: Record<string, string> = {
  platform: "§2",
  auth: "§2",
  me: "§2",
  "me.streamTicket": "§6",
  sources: "§4",
  links: "§5",
  relay: "§17",
  "auth.changePassword": "§18",
  "me.traffic": "§19",
  teams: "§21",
  invitations: "§22",
  groups: "§23",
  grants: "§24",
  accessRequests: "§24",
  audit: "§25",
  billing: "§26",
  apiTokens: "§27",
  admin: "§28",
  status: "§28",
  trust: "§31",
  "sources.trustRequest": "§31",
  "sources.trustResolve": "§31",
  "auth.deviceGet": "§29",
  "auth.deviceDeny": "§29",
  "me.notificationPrefs": "§29",
  "me.notificationPrefsUpdate": "§29",
  "auth.emailVerify": "§29",
  "auth.emailVerifyResend": "§29",
  "auth.passwordResetStart": "§29",
  "auth.passwordResetFinish": "§29",
  "auth.oauthStart": "§29",
  "auth.oauthFinish": "§29",
  "auth.oauthUnlink": "§29",
  "me.emailChange": "§29",
  "me.spaces": "§29",
  "me.securityLog": "§29",
  "me.delete": "§29",
  "me.deleteCancel": "§29",
  "me.export": "§29",
};

export function sectionOf(key: string): string {
  return (
    CONTRACT_SECTIONS[key] ?? CONTRACT_SECTIONS[key.split(".")[0] ?? ""] ?? ""
  );
}

export interface ProcedureEntry {
  /** `auth.login` 这样的点分键。 */
  key: string;
  method: string;
  path: string;
  meta: CloudMeta;
  errorCodes: string[];
  procedure: AnyContractProcedure;
}

/** 按声明顺序列出全部 procedure。 */
export function listProcedures(
  router: object = cloudContract,
  prefix = "",
): ProcedureEntry[] {
  const out: ProcedureEntry[] = [];
  for (const [name, value] of Object.entries(router)) {
    const key = prefix === "" ? name : `${prefix}.${name}`;
    if (isContractProcedure(value)) {
      const def = value["~orpc"];
      out.push({
        key,
        method: def.route.method ?? "POST",
        path: def.route.path ?? "",
        meta: def.meta as CloudMeta,
        errorCodes: Object.keys(def.errorMap as ErrorMap),
        procedure: value,
      });
    } else if (value !== null && typeof value === "object") {
      out.push(...listProcedures(value, key));
    }
  }
  return out;
}

export {
  ME_STREAM_PATH,
  ME_STREAM_PING_MS,
  meStreamClientMessageSchema,
  meStreamEventSchema,
} from "./events.ts";
export type { MeStreamClientMessage, MeStreamEvent } from "./events.ts";
export { SCOPES, type CloudMeta, type Scope } from "./meta.ts";
export * from "./schemas.ts";
export * from "./saas-schemas.ts";
export {
  deviceStartOutputSchema,
  devicePollOutputSchema,
  OAUTH_PROVIDERS,
  oauthProviderSchema,
  passkeySchema,
} from "./contract/auth.ts";
export {
  adminAccountSchema,
  adminTenantSchema,
  adminIncidentSchema,
  adminRelayNodeSchema,
  adminTeamSchema,
} from "./contract/admin.ts";
export { billingPlanSchema, billingUsageSchema } from "./contract/billing.ts";
export { createLinkInputSchema } from "./contract/links.ts";
export {
  meDeviceSchema,
  securityEventSchema,
  TRAFFIC_GRANULARITIES,
  TRAFFIC_KINDS,
  TRAFFIC_MAX_ROWS,
  trafficBucketSchema,
  trafficInputSchema,
  trafficKindSchema,
} from "./contract/me.ts";
export {
  PLATFORM_CAPABILITIES,
  PLATFORM_NAME_MAX,
  platformChallengeSchema,
  platformHealthSchema,
  platformInfoSchema,
  platformModeSchema,
} from "./contract/platform.ts";
export {
  relayNodeIdSchema,
  relayPresenceEventSchema,
  type RelayPresenceEvent,
} from "./contract/relay.ts";
export {
  relayNodeSchema,
  sourceAccessSchema,
  sourceAssertionOutputSchema,
} from "./contract/sources.ts";
