/**
 * 记录层（设计 §4.5）。通道上的字节流由「单元」组成：
 *
 * ```text
 * offset  size  字段
 * 0       4     length (u32 BE)
 * 4       n     体：握手消息（m1 / m2 / m3），或记录密文
 * ```
 *
 * 记录 = AES-256-GCM(key = k_dir, nonce = salt_dir(4) || counter(8 BE), aad = 长度字段,
 * plaintext = 条目序列)。条目是 `u32 BE 长度 || 字节`，一般是一个完整的内层隧道帧；长度 0
 * 是填充标记，其后到明文末尾全是 0（只在 m1 开了填充时出现，补到 64 字节的倍数）。
 *
 * - 每个方向独立计数器，从 0 起；到 2³² 抛 `record_limit`，由客户端重建通道。
 * - 中继可以合并或切分 WS 消息，{@link UnitReader} 按长度重新切；长度超限立即失败。
 * - 重放、乱序、截断、改长度都会让 AEAD 失败（计数器 nonce + 长度进 aad）；打开失败后
 *   这个方向就不能再用，调用方必须关闭通道。
 */
import { decodeFrame, encodeFrame, type Frame } from "../tunnel/frames.ts";
import { LIMITS_E2E } from "./constants.ts";
import type { E2eDirectionSecret, E2eSession } from "./handshake.ts";
import {
  aeadOpen,
  aeadSeal,
  concatBytes,
  E2eError,
  importAeadKey,
  type AeadKey,
} from "./primitives.ts";

const HEADER = LIMITS_E2E.recordHeaderBytes;

function u32(value: number): Uint8Array {
  const out = new Uint8Array(4);
  new DataView(out.buffer).setUint32(0, value, false);
  return out;
}

function readU32(bytes: Uint8Array, at: number): number {
  return new DataView(bytes.buffer, bytes.byteOffset + at, 4).getUint32(
    0,
    false,
  );
}

/** 一个单元：长度 + 体（握手消息用；记录由 {@link RecordSealer} 产出）。 */
export function encodeUnit(body: Uint8Array): Uint8Array {
  if (body.length > LIMITS_E2E.maxRecordCiphertext)
    throw new E2eError(
      "record_too_large",
      `单元体 ${body.length} 字节超过上限`,
    );
  return concatBytes(u32(body.length), body);
}

/** 单元的体（去掉 4 字节长度）。 */
export function unitBody(unit: Uint8Array): Uint8Array {
  return unit.subarray(HEADER);
}

/**
 * 把任意切分的字节流重新切成单元。`maxBody` 握手阶段设成
 * `maxHandshakeMessageBytes`，握手完成后放宽到 `maxRecordCiphertext`。
 */
export class UnitReader {
  private buffered: Uint8Array = new Uint8Array(0);

  private maxBody: number;

  constructor(maxBody: number = LIMITS_E2E.maxRecordCiphertext) {
    this.maxBody = maxBody;
  }

  setMaxBody(value: number): void {
    this.maxBody = value;
  }

  /** 缓冲里还没凑成单元的字节数。 */
  get pending(): number {
    return this.buffered.length;
  }

  /** 吃一段字节，答这段里凑齐的单元（含长度字段）。 */
  feed(chunk: Uint8Array): Uint8Array[] {
    this.buffered =
      this.buffered.length === 0 ? chunk : concatBytes(this.buffered, chunk);
    const units: Uint8Array[] = [];
    while (this.buffered.length >= HEADER) {
      const length = readU32(this.buffered, 0);
      if (length > this.maxBody)
        throw new E2eError("record_too_large", `单元体 ${length} 字节超过上限`);
      if (this.buffered.length < HEADER + length) break;
      units.push(this.buffered.slice(0, HEADER + length));
      this.buffered = this.buffered.subarray(HEADER + length);
    }
    if (this.buffered.length === 0) this.buffered = new Uint8Array(0);
    return units;
  }
}

// ------------------------------------------------------------------ 条目

/** 条目序列 → 记录明文；`padding` 时补到 64 字节的倍数。 */
export function packEntries(
  entries: readonly Uint8Array[],
  padding: boolean,
): Uint8Array {
  let total = 0;
  for (const entry of entries) {
    if (entry.length === 0) throw new E2eError("bad_record", "条目不能为空");
    total += HEADER + entry.length;
  }
  if (total > LIMITS_E2E.maxRecordEntries)
    throw new E2eError("record_too_large", `条目共 ${total} 字节超过一条记录`);
  const block = LIMITS_E2E.padBlock;
  const padded =
    padding && total % block !== 0
      ? Math.ceil((total + HEADER) / block) * block
      : total;
  const out = new Uint8Array(padded);
  let at = 0;
  for (const entry of entries) {
    out.set(u32(entry.length), at);
    out.set(entry, at + HEADER);
    at += HEADER + entry.length;
  }
  return out;
}

/** 记录明文 → 条目序列；结构不对抛 `bad_record`。 */
export function unpackEntries(
  plaintext: Uint8Array,
  padding: boolean,
): Uint8Array[] {
  const entries: Uint8Array[] = [];
  let at = 0;
  while (at < plaintext.length) {
    if (plaintext.length - at < HEADER)
      throw new E2eError("bad_record", "条目长度被截断");
    const length = readU32(plaintext, at);
    if (length === 0) {
      if (!padding) throw new E2eError("bad_record", "没有协商填充");
      for (let index = at + HEADER; index < plaintext.length; index += 1)
        if (plaintext[index] !== 0)
          throw new E2eError("bad_record", "填充不是全零");
      if (plaintext.length % LIMITS_E2E.padBlock !== 0)
        throw new E2eError("bad_record", "填充没有对齐");
      return entries;
    }
    if (at + HEADER + length > plaintext.length)
      throw new E2eError("bad_record", "条目越界");
    entries.push(plaintext.slice(at + HEADER, at + HEADER + length));
    at += HEADER + length;
  }
  return entries;
}

function encodeInner(frames: readonly Frame[]): Uint8Array[] {
  return frames.map((frame) => {
    const bytes = encodeFrame(frame);
    if (bytes.length > LIMITS_E2E.maxInnerFrameBytes)
      throw new E2eError(
        "record_too_large",
        `内层帧 ${bytes.length} 字节超过上限（DATA 须 ≤ ${LIMITS_E2E.maxInnerDataChunk}）`,
      );
    return bytes;
  });
}

// ------------------------------------------------------------------ 密封与打开

function nonceFor(salt: Uint8Array, counter: number): Uint8Array {
  const nonce = new Uint8Array(12);
  nonce.set(salt, 0);
  const view = new DataView(nonce.buffer);
  view.setUint32(4, Math.floor(counter / 2 ** 32), false);
  view.setUint32(8, counter >>> 0, false);
  return nonce;
}

abstract class Direction {
  protected counter = 0;
  protected tail: Promise<unknown> = Promise.resolve();

  protected readonly key: AeadKey;
  protected readonly salt: Uint8Array;
  readonly padding: boolean;

  protected constructor(key: AeadKey, salt: Uint8Array, padding: boolean) {
    this.key = key;
    this.salt = salt;
    this.padding = padding;
    if (salt.length !== 4)
      throw new E2eError("bad_record", "nonce 前缀须为 4 字节");
  }

  /** 这个方向已用掉的记录数（下一条记录的计数器）。 */
  get records(): number {
    return this.counter;
  }

  protected next(): number {
    if (this.counter >= LIMITS_E2E.maxRecordsPerDirection)
      throw new E2eError("record_limit", "记录数到上限，须重建通道");
    const counter = this.counter;
    this.counter += 1;
    return counter;
  }

  /** 按调用顺序完成（结果的先后与计数器一致）。 */
  protected chain<T>(task: () => Promise<T>): Promise<T> {
    const result = this.tail.then(task);
    this.tail = result.catch(() => undefined);
    return result;
  }
}

/** 一个方向的发送端。 */
export class RecordSealer extends Direction {
  static async create(
    secret: E2eDirectionSecret,
    options: { padding: boolean },
  ): Promise<RecordSealer> {
    return new RecordSealer(
      await importAeadKey(secret.key),
      secret.salt.slice(),
      options.padding,
    );
  }

  private constructor(key: AeadKey, salt: Uint8Array, padding: boolean) {
    super(key, salt, padding);
  }

  /** 条目 → 一个完整单元（长度 + 密文）。 */
  seal(entries: readonly Uint8Array[]): Promise<Uint8Array> {
    const plaintext = packEntries(entries, this.padding);
    const counter = this.next();
    return this.chain(async () => {
      const header = u32(plaintext.length + LIMITS_E2E.tagBytes);
      const sealed = await aeadSeal(
        this.key,
        nonceFor(this.salt, counter),
        header,
        plaintext,
      );
      return concatBytes(header, sealed);
    });
  }

  /** 内层隧道帧 → 一个单元。 */
  sealFrames(frames: readonly Frame[]): Promise<Uint8Array> {
    return this.seal(encodeInner(frames));
  }
}

/** 一个方向的接收端：单元必须按收到的顺序喂进来。 */
export class RecordOpener extends Direction {
  private failed = false;

  static async create(
    secret: E2eDirectionSecret,
    options: { padding: boolean },
  ): Promise<RecordOpener> {
    return new RecordOpener(
      await importAeadKey(secret.key),
      secret.salt.slice(),
      options.padding,
    );
  }

  private constructor(key: AeadKey, salt: Uint8Array, padding: boolean) {
    super(key, salt, padding);
  }

  /** 一个完整单元 → 条目。失败后这个方向作废。 */
  open(unit: Uint8Array): Promise<Uint8Array[]> {
    if (this.failed)
      return Promise.reject(new E2eError("decrypt_failed", "通道已作废"));
    if (unit.length < HEADER + LIMITS_E2E.tagBytes) {
      this.failed = true;
      return Promise.reject(new E2eError("bad_record", "记录过短"));
    }
    const length = readU32(unit, 0);
    if (
      length !== unit.length - HEADER ||
      length > LIMITS_E2E.maxRecordCiphertext
    ) {
      this.failed = true;
      return Promise.reject(new E2eError("record_too_large", "记录长度不对"));
    }
    const counter = this.next();
    return this.chain(async () => {
      try {
        const plaintext = await aeadOpen(
          this.key,
          nonceFor(this.salt, counter),
          unit.subarray(0, HEADER),
          unit.subarray(HEADER),
        );
        return unpackEntries(plaintext, this.padding);
      } catch (error) {
        this.failed = true;
        throw error;
      }
    });
  }

  /** 一个完整单元 → 内层隧道帧。 */
  async openFrames(unit: Uint8Array): Promise<Frame[]> {
    return (await this.open(unit)).map((entry) => decodeFrame(entry));
  }
}

/** 一端的记录层：自己发的方向与对端发的方向。 */
export interface RecordLayer {
  sealer: RecordSealer;
  opener: RecordOpener;
}

/** 按角色从会话建记录层：客户端发 c2s、收 s2c，core 反过来。 */
export async function createRecordLayer(
  session: E2eSession,
  role: "client" | "core",
): Promise<RecordLayer> {
  const send = role === "client" ? session.clientToCore : session.coreToClient;
  const receive =
    role === "client" ? session.coreToClient : session.clientToCore;
  const options = { padding: session.padding };
  return {
    sealer: await RecordSealer.create(send, options),
    opener: await RecordOpener.create(receive, options),
  };
}
