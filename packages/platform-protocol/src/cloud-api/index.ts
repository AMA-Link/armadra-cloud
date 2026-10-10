/**
 * `/v1/*` 契约（协议包设计 §5）。cloud 用 `implement(cloudContract)`；页面 / 手机 / core
 * 用 `createORPCClient(new OpenAPILink(cloudContract, { url }))`。
 */
import {
  isContractProcedure,
  type AnyContractProcedure,
  type ErrorMap,
} from "@orpc/contract";

import { auth } from "./contract/auth.ts";
import { billing } from "./contract/billing.ts";
import { links } from "./contract/links.ts";
import { me } from "./contract/me.ts";
import { orgs } from "./contract/orgs.ts";
import { platform } from "./contract/platform.ts";
import { relay } from "./contract/relay.ts";
import { sources } from "./contract/sources.ts";
import type { CloudMeta } from "./meta.ts";

export const cloudContract = {
  platform,
  auth,
  me,
  sources,
  links,
  orgs,
  billing,
  relay,
};
export type CloudContract = typeof cloudContract;

/** `docs/contracts/cloud-api.md` 的节号：按顶层域，`me.streamTicket` 归 §6、`auth.changePassword` 归 §18。 */
export const CONTRACT_SECTIONS: Record<string, string> = {
  platform: "§2",
  auth: "§2",
  me: "§2",
  "me.streamTicket": "§6",
  orgs: "§3",
  sources: "§4",
  links: "§5",
  billing: "§9",
  relay: "§17",
  "auth.changePassword": "§18",
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
export {
  deviceStartOutputSchema,
  devicePollOutputSchema,
  passkeySchema,
} from "./contract/auth.ts";
export { billingPlanSchema, billingUsageSchema } from "./contract/billing.ts";
export { createLinkInputSchema } from "./contract/links.ts";
export { meDeviceSchema } from "./contract/me.ts";
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
