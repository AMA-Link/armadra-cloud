/**
 * 协议版本：隧道握手 `hello.protocol`、`platform.info.protocol`、`/v1` 响应头
 * `Armadra-Protocol` 都报（断言 `ver` 只报 major）。
 *
 * - 1.0（协议包 0.1–0.2）；
 * - 1.1（0.3.0）：断言 `scp` / `role` / `team`，中继令牌 `org` / `ro`，隧道令牌 `org`，
 *   `links.create` 的 `scope`。都是可选的新声明，缺省 = 1.0 的行为。
 * - 1.2（0.3.5）：挑战令牌（`auth.login` / `links.accept` 的 `challenge`、`platform.info.challenge`，
 *   cloud-api §16）；中继节点地址可带 query、节点回报 `relay.nodePresence`（§17）。都是可选的新字段。
 * - 1.3（0.4.0）：端到端通道（cloud-api §20）——隧道 `OPEN.kind` 加 `e2e`、源能力 `e2e`、
 *   中继令牌可选声明 `dev`；通道本身的握手与记录层在 `./e2e`（`armadra-e2e/1`）。老源不报
 *   `e2e` 就收不到这种 `OPEN`，老中继不认 `dev` 照常放行。
 * - 1.4（0.10.0）：SaaS 租户是团队（cloud-api §21 起）——断言 `team` 改为 `{ teamId, role:
 *   owner|admin|member|billing|guest }`（1.1 的部门语义从未签发过），中继令牌与隧道令牌的
 *   `org` 换成 `team`（团队 id）；契约删去从未实现的 `orgs.*`，追加 `teams` / `invitations` /
 *   `groups` / `grants` / `accessRequests` / `audit` / `billing` / `apiTokens` / `admin` 等
 *   预留 procedure。`org` 声明保留给以后的企业组织层。
 */
export const PROTOCOL_VERSION = { major: 1, minor: 4 } as const;

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
/** 响应头 `Armadra-Protocol: 1.1`。 */
export const PROTOCOL_HEADER = "armadra-protocol";
export const PLATFORM_WELL_KNOWN = "/.well-known/armadra-platform";
export const JWKS_WELL_KNOWN = "/.well-known/jwks.json";

/** `Armadra-Protocol` 头的值。 */
export function protocolHeaderValue(
  version: { major: number; minor: number } = PROTOCOL_VERSION,
): string {
  return `${version.major}.${version.minor}`;
}
