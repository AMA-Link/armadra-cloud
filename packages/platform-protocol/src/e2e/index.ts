// 端到端通道 `armadra-e2e/1`（中继端到端加密设计 §4）：握手、记录层与 r0。
// 原语全部来自 WebCrypto，页面、手机、Node 与 Workers 同一份代码。
export {
  decodeAccept,
  E2E_DEVICE_STATUSES,
  e2eAcceptSchema,
  e2eDeviceStatusSchema,
  encodeAccept,
  type E2eAccept,
  type E2eDeviceStatus,
} from "./accept.ts";
export {
  E2E_CAPABILITY,
  E2E_FLAGS,
  E2E_LABELS,
  E2E_PATH,
  E2E_PROTOCOL,
  E2E_SUBPROTOCOL,
  E2E_VERSION,
  LIMITS_E2E,
  type E2eLimits,
} from "./constants.ts";
export {
  ClientHandshake,
  CoreHandshake,
  type ClientFinishOptions,
  type ClientHandshakeOptions,
  type CoreHandshakeOptions,
  type CoreHandshakeResult,
  type E2eDevice,
  type E2eDirectionSecret,
  type E2eSession,
} from "./handshake.ts";
export {
  decodeDevice,
  decodeHello,
  deviceIdOf,
  e2eDeviceSchema,
  e2eHelloSchema,
  encodeDevice,
  encodeHello,
  identityFingerprint,
  M1_BYTES,
  M2_BYTES,
  pinHint,
  type E2eDeviceMessage,
  type E2eHello,
} from "./messages.ts";
export {
  E2eError,
  ed25519Signer,
  ed25519SignerFromSeed,
  ed25519Verify,
  generateX25519,
  x25519FromSeed,
  type E2eErrorCode,
  type E2eSigner,
  type X25519KeyPair,
} from "./primitives.ts";
export {
  createRecordLayer,
  encodeUnit,
  packEntries,
  RecordOpener,
  RecordSealer,
  UnitReader,
  unitBody,
  unpackEntries,
  type RecordLayer,
} from "./records.ts";
