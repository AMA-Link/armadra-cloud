/**
 * 端到端通道 `armadra-e2e/1` 的常量（中继端到端加密设计 §4）。
 *
 * 套件固定、不协商：X25519、HKDF-SHA256、AES-256-GCM、Ed25519、SHA-256；只协商版本
 * （边缘子协议名 {@link E2E_SUBPROTOCOL}）。
 */

/** 握手消息里的版本字节。 */
export const E2E_VERSION = 1;
/** 转录哈希的起点与全部派生标签的前缀。 */
export const E2E_PROTOCOL = "armadra-e2e/1";
/** 客户端连 `_relay/e2e` 时带的版本子协议（与 `armadra-relay.<令牌>` 一起）。 */
export const E2E_SUBPROTOCOL = "armadra-e2e.1";
/** 边缘地址里的保留路径：saas `/_relay/e2e`，personal `/s/<sourceId>/_relay/e2e`。 */
export const E2E_PATH = "/_relay/e2e";
/** 源在隧道 `hello.capabilities` 里报这一项，中继才对它开 `_relay/e2e`。 */
export const E2E_CAPABILITY = "e2e";

/** m1 的 `flags` 位；未定义的位必须为 0。 */
export const E2E_FLAGS = {
  /** 记录明文填充到 64 字节的倍数（缺省关，只缓解按大小的流量分析）。 */
  padding: 0x01,
} as const;

/** HKDF `info`、签名域与证明的标签；都以 {@link E2E_PROTOCOL} 开头，互不为前缀。 */
export const E2E_LABELS = {
  handshakeClientToCore: `${E2E_PROTOCOL} hs c2s`,
  handshakeCoreToClient: `${E2E_PROTOCOL} hs s2c`,
  clientToCore: `${E2E_PROTOCOL} c2s`,
  coreToClient: `${E2E_PROTOCOL} s2c`,
  sas: `${E2E_PROTOCOL} sas`,
  proof: `${E2E_PROTOCOL} proof`,
  coreSignature: `${E2E_PROTOCOL} core`,
  deviceSignature: `${E2E_PROTOCOL} device`,
} as const;

const RECORD_HEADER_BYTES = 4;
const TAG_BYTES = 16;
const MAX_RECORD_PLAINTEXT = 65_536;
/** 记录明文里每个条目的长度前缀。 */
const ENTRY_HEADER_BYTES = 4;
/** 隧道帧头（type + streamId）。 */
const FRAME_HEADER_BYTES = 5;
/** 条目总长上限：给填充标记留 4 字节，开不开填充都一样。 */
const MAX_RECORD_ENTRIES = MAX_RECORD_PLAINTEXT - ENTRY_HEADER_BYTES;

export const LIMITS_E2E = {
  /** 每个单元（握手消息或记录）前面的 u32 BE 长度。 */
  recordHeaderBytes: RECORD_HEADER_BYTES,
  /** AES-GCM 标签。 */
  tagBytes: TAG_BYTES,
  /** 一条记录的明文上限（64 KiB）。 */
  maxRecordPlaintext: MAX_RECORD_PLAINTEXT,
  /** 一条记录的密文上限（明文 + 标签）= 长度字段的上限。 */
  maxRecordCiphertext: MAX_RECORD_PLAINTEXT + TAG_BYTES,
  /** 一条记录里全部条目（含各自 4 字节长度）的上限。 */
  maxRecordEntries: MAX_RECORD_ENTRIES,
  /** 一个内层帧（一个条目的载荷）的上限。 */
  maxInnerFrameBytes: MAX_RECORD_ENTRIES - ENTRY_HEADER_BYTES,
  /** 内层 `DATA` 的载荷上限（比隧道的 `maxDataChunk` 小，整帧才装得进一条记录）。 */
  maxInnerDataChunk:
    MAX_RECORD_ENTRIES - ENTRY_HEADER_BYTES - FRAME_HEADER_BYTES,
  /** 握手消息体的上限（m3 最长 243 字节）。 */
  maxHandshakeMessageBytes: 1024,
  /** 每个方向的记录数上限（计数器 2³²）；到了由客户端重建通道，不做在线重键。 */
  maxRecordsPerDirection: 2 ** 32,
  /** 通道最长存活；到了由客户端重建。 */
  maxChannelAgeMs: 3_600_000,
  handshakeTimeoutMs: 10_000,
  /** 内层 `PING` 的间隔（保活兼 RTT）。 */
  pingIntervalMs: 20_000,
  /** 填充块大小。 */
  padBlock: 64,
  /** m3 里 `platform` 与 `name` 的 UTF-8 字节上限。 */
  platformMaxBytes: 32,
  nameMaxBytes: 64,
} as const;

export type E2eLimits = typeof LIMITS_E2E;
