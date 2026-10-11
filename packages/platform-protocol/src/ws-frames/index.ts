// WebSocket 帧的纯编解码（RFC 6455 §5）。黄金字节在 `fixtures/ws/`。
export {
  closeFrame,
  closePayload,
  encodeWsFrame,
  FrameBoundaryTracker,
  OPCODE,
  PING_FRAME,
  WsFrameDecoder,
  WsFrameError,
  type WsEvent,
  type WsFrameDecoderOptions,
  type WsFrameErrorReason,
  type WsFrameInput,
} from "./frames.ts";
