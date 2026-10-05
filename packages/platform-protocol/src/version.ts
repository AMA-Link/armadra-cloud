/** 协议版本：隧道握手 `hello.protocol`、断言 `ver`、`/v1` 响应头 `Armadra-Protocol` 三处都报。 */
export const PROTOCOL_VERSION = { major: 1, minor: 0 } as const;

/** 隧道路径；major 变化时同时升为 `/t/v2`。 */
export const TUNNEL_PATH = "/t/v1";
/** saas 节点间转发。 */
export const INTERNODE_PATH = "/x/v1";
/** 客户端 WS 的中继令牌子协议：`armadra-relay.<jwt>`。 */
export const RELAY_SUBPROTOCOL_PREFIX = "armadra-relay.";
/** WS 票据子协议，与 core 现有常量相同：`armadra-ticket.<票>`。 */
export const TICKET_SUBPROTOCOL_PREFIX = "armadra-ticket.";
/** 客户端 HTTP 的中继令牌头。 */
export const RELAY_TOKEN_HEADER = "armadra-relay-token";
/** 响应头 `Armadra-Protocol: 1.0`。 */
export const PROTOCOL_HEADER = "armadra-protocol";
export const PLATFORM_WELL_KNOWN = "/.well-known/armadra-platform";
export const JWKS_WELL_KNOWN = "/.well-known/jwks.json";

/** `Armadra-Protocol` 头的值。 */
export function protocolHeaderValue(
  version: { major: number; minor: number } = PROTOCOL_VERSION,
): string {
  return `${version.major}.${version.minor}`;
}
