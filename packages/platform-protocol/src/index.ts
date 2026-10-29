// 包根只 re-export 版本常量与公共类型；实现从子路径 import：
//   ./tunnel ./assertion ./cloud-api ./core-api ./errors ./identity-vectors ./fixtures（Node，测试用）
export {
  INTERNODE_PATH,
  JWKS_WELL_KNOWN,
  MIN_CORE_VERSION_ROLE_ONLY_ASSERTION,
  MIN_CORE_VERSION_TEAM_ASSERTION,
  PLATFORM_WELL_KNOWN,
  PROTOCOL_HEADER,
  PROTOCOL_VERSION,
  protocolHeaderValue,
  RELAY_SUBPROTOCOL_PREFIX,
  RELAY_TOKEN_HEADER,
  TICKET_SUBPROTOCOL_PREFIX,
  TUNNEL_PATH,
} from "./version.ts";
export type { Frame, OpenPayload, TunnelRejectCode } from "./tunnel/index.ts";
export type {
  AccessClaims,
  AssertionClaims,
  Ed25519PublicJwk,
  JwkSet,
  NodeTokenClaims,
  RelayTokenClaims,
  SourceJwsClaims,
  TunnelTokenClaims,
} from "./assertion/index.ts";
export type {
  CloudContract,
  CloudMeta,
  MeStreamEvent,
  Scope,
  Session,
  SourceSummary,
} from "./cloud-api/index.ts";
export type {
  ClientSource,
  CloudLoginOutput,
  RemoteService,
  TunnelStatus,
} from "./core-api/index.ts";
export type {
  ErrorCode,
  LimitReachedDetails,
  Quota,
  SourceCoreOutdatedDetails,
  TeamHostsOnlineDetails,
  WireError,
} from "./errors/index.ts";
