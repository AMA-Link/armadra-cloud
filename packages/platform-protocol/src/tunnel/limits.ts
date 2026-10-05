/** 隧道限制（协议包设计 §3.3）。ws 的 maxPayload = maxFrameBytes + 16。 */
export const LIMITS = {
  /** 整帧上限（含 5 字节帧头）。 */
  maxFrameBytes: 1_048_576,
  /** 单个 DATA 帧的载荷上限。 */
  maxDataChunk: 65_536,
  /** 每流初始信用（256 KiB）。 */
  streamWindow: 262_144,
  /** 隧道级初始信用（8 MiB）。 */
  tunnelWindow: 8_388_608,
  maxStreams: 256,
  heartbeatMs: 20_000,
  heartbeatMisses: 2,
  /** OPEN 载荷里 headers 的 UTF-8 总字节上限。 */
  openHeadersMaxBytes: 32_768,
  handshakeTimeoutMs: 10_000,
  goawayGraceMs: 30_000,
} as const;

export type TunnelLimits = typeof LIMITS;
