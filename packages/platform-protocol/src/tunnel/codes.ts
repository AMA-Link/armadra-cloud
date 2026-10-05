/** RST 码（u16）。 */
export const RST = {
  ok: 0,
  protocol: 1,
  refused: 2,
  sourceGone: 3,
  internal: 4,
  flowControl: 5,
  clientGone: 6,
  timeout: 7,
} as const;

export type RstCode = (typeof RST)[keyof typeof RST];

/**
 * WebSocket 关闭码。客户端侧（浏览器 / 手机）只会看到前七个 4xxx；449x 只在隧道上。
 */
export const CLOSE = {
  normal: 1000,
  goingAway: 1001,
  badFrame: 4400,
  unauthenticated: 4401,
  forbidden: 4403,
  sourceOffline: 4404,
  protocolUnsupported: 4409,
  frameTooLarge: 4413,
  limitReached: 4429,
  tunnelAuthFailed: 4490,
  tunnelReplaced: 4491,
  tunnelGoaway: 4492,
} as const;

export type CloseCode = (typeof CLOSE)[keyof typeof CLOSE];

/** 帧类型字节。 */
export const FRAME_TYPE = {
  open: 1,
  data: 2,
  end: 3,
  rst: 4,
  window: 5,
  ping: 6,
  pong: 7,
  goaway: 8,
} as const;

export type FrameTypeName = keyof typeof FRAME_TYPE;
