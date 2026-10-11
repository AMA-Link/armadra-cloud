import { describe, expect, it } from "vitest";

import {
  ClientHandshake,
  CoreHandshake,
  createRecordLayer,
  decodeAccept,
  decodeDevice,
  decodeHello,
  E2E_FLAGS,
  E2eError,
  ed25519SignerFromSeed,
  encodeAccept,
  encodeUnit,
  identityFingerprint,
  LIMITS_E2E,
  M1_BYTES,
  packEntries,
  pinHint,
  UnitReader,
  unitBody,
  unpackEntries,
  x25519FromSeed,
  type E2eAccept,
  type E2eSession,
  type E2eSigner,
} from "../src/e2e/index.ts";
import { fromHex, toHex } from "../src/e2e/primitives.ts";
import { readJsonFixture } from "../src/fixtures/index.ts";
import { decodeFrame, type Frame } from "../src/tunnel/frames.ts";

const SOURCE_ID = "0123456789abcdef0123456789abcdef";

interface Vector {
  name: string;
  inputs: {
    sourceId: string;
    flags: number;
    pinned: boolean;
    clientEphemeralSeed: string;
    coreEphemeralSeed: string;
    coreIdentitySeed: string;
    deviceSeed: string;
    nonceC: string;
    platform: "browser" | "ios";
    deviceName: string;
    proofSecret: string | null;
  };
  keys: Record<
    | "clientEphemeralPub"
    | "coreEphemeralPub"
    | "coreIdentityPub"
    | "devicePub"
    | "coreFingerprint"
    | "pinHint"
    | "deviceId",
    string
  >;
  transcript: Record<"h0" | "h1" | "h2" | "h3" | "h4", string>;
  secrets: {
    clientToCore: { key: string; salt: string };
    coreToClient: { key: string; salt: string };
    proof: string | null;
  };
  sas: string;
  messages: { m1: string; m2: string; m3: string };
  records: {
    accept: { value: E2eAccept; unit: string };
    clientFrames: string[];
    client: string[];
  };
}

const { vectors } = readJsonFixture<{ vectors: Vector[] }>(
  "e2e/handshake.json",
);

function e2eError(code: string) {
  return expect.objectContaining({ name: "E2eError", code });
}

async function run(vector: Vector) {
  const { inputs } = vector;
  const identity = await ed25519SignerFromSeed(
    fromHex(inputs.coreIdentitySeed),
  );
  const device = await ed25519SignerFromSeed(fromHex(inputs.deviceSeed));
  const client = await ClientHandshake.start({
    sourceId: inputs.sourceId,
    flags: inputs.flags,
    pinnedIdentity: inputs.pinned ? identity.publicKey : null,
    ephemeral: await x25519FromSeed(fromHex(inputs.clientEphemeralSeed)),
    nonce: fromHex(inputs.nonceC),
  });
  const core = await CoreHandshake.accept(client.m1, {
    sourceId: inputs.sourceId,
    identity,
    ephemeral: await x25519FromSeed(fromHex(inputs.coreEphemeralSeed)),
  });
  const { coreIdentity } = await client.readCore(core.m2);
  const proofSecret = inputs.proofSecret
    ? fromHex(inputs.proofSecret)
    : undefined;
  const finished = await client.finish({
    device,
    platform: inputs.platform,
    name: inputs.deviceName,
    ...(proofSecret ? { proofSecret } : {}),
  });
  const result = await core.readDevice(finished.m3);
  return {
    identity,
    device,
    client,
    core,
    coreIdentity,
    finished,
    result,
    proofSecret,
  };
}

function sessionHex(session: E2eSession) {
  return {
    clientToCore: {
      key: toHex(session.clientToCore.key),
      salt: toHex(session.clientToCore.salt),
    },
    coreToClient: {
      key: toHex(session.coreToClient.key),
      salt: toHex(session.coreToClient.salt),
    },
  };
}

describe("e2e 黄金向量（node:crypto 参照实现）", () => {
  it("有两组向量：首次配对带 proof、钉扎加填充", () => {
    expect(vectors.map((vector) => vector.name)).toEqual([
      "pairing",
      "pinned-padded",
    ]);
  });

  for (const vector of vectors) {
    describe(vector.name, () => {
      it("三条消息、转录、会话密钥与 SAS 逐字节相同", async () => {
        const { client, core, coreIdentity, finished, result } =
          await run(vector);
        expect(toHex(client.m1)).toBe(vector.messages.m1);
        expect(toHex(core.m2)).toBe(vector.messages.m2);
        expect(toHex(finished.m3)).toBe(vector.messages.m3);
        expect(toHex(coreIdentity)).toBe(vector.keys.coreIdentityPub);
        expect(toHex(finished.session.transcript)).toBe(vector.transcript.h4);
        expect(toHex(result.session.transcript)).toBe(vector.transcript.h4);
        const expected = {
          clientToCore: vector.secrets.clientToCore,
          coreToClient: vector.secrets.coreToClient,
        };
        expect(sessionHex(finished.session)).toEqual(expected);
        expect(sessionHex(result.session)).toEqual(expected);
        expect(finished.session.sas).toBe(vector.sas);
        expect(result.session.sas).toBe(vector.sas);
        expect(result.session.padding).toBe(
          (vector.inputs.flags & E2E_FLAGS.padding) !== 0,
        );
      });

      it("身份标识：指纹、pinHint、设备 id", async () => {
        const { identity, result } = await run(vector);
        expect(await identityFingerprint(identity.publicKey)).toBe(
          vector.keys.coreFingerprint,
        );
        expect(toHex(await pinHint(identity.publicKey))).toBe(
          vector.keys.pinHint,
        );
        expect(result.device).toEqual({
          publicKey: fromHex(vector.keys.devicePub),
          deviceId: vector.keys.deviceId,
          platform: vector.inputs.platform,
          name: vector.inputs.deviceName,
        });
        expect(vector.keys.coreFingerprint).toHaveLength(52);
      });

      it("proof：对的秘密通过，错的与缺的不通过", async () => {
        const { result, proofSecret } = await run(vector);
        expect(result.hasProof).toBe(proofSecret !== undefined);
        if (proofSecret) {
          expect(await result.verifyProof(proofSecret)).toBe(true);
          const wrong = proofSecret.slice();
          wrong[0] = (wrong[0] as number) ^ 1;
          expect(await result.verifyProof(wrong)).toBe(false);
        } else {
          expect(await result.verifyProof(new Uint8Array(16))).toBe(false);
        }
      });

      it("记录：r0 与客户端头两条记录逐字节相同，对端能打开", async () => {
        const { finished, result } = await run(vector);
        const core = await createRecordLayer(result.session, "core");
        const client = await createRecordLayer(finished.session, "client");
        const acceptUnit = await core.sealer.seal([
          encodeAccept(vector.records.accept.value),
        ]);
        expect(toHex(acceptUnit)).toBe(vector.records.accept.unit);
        expect(decodeAccept(await client.opener.open(acceptUnit))).toEqual(
          vector.records.accept.value,
        );
        const frames = vector.records.clientFrames.map((value) =>
          decodeFrame(fromHex(value)),
        );
        for (const [counter, expected] of vector.records.client.entries()) {
          const unit = await client.sealer.sealFrames(frames);
          expect(toHex(unit), `计数器 ${counter}`).toBe(expected);
          expect(await core.opener.openFrames(unit)).toEqual(frames);
        }
        expect(client.sealer.records).toBe(2);
        expect(core.opener.records).toBe(2);
      });
    });
  }
});

// ------------------------------------------------------------------ 握手

async function pair(options: { pinned?: boolean; flags?: number } = {}) {
  const identity = await ed25519SignerFromSeed(new Uint8Array(32).fill(7));
  const device = await ed25519SignerFromSeed(new Uint8Array(32).fill(8));
  const client = await ClientHandshake.start({
    sourceId: SOURCE_ID,
    pinnedIdentity: options.pinned ? identity.publicKey : null,
    flags: options.flags ?? 0,
  });
  return { identity, device, client };
}

function flip(bytes: Uint8Array, index: number): Uint8Array {
  const out = bytes.slice();
  out[index] = (out[index] as number) ^ 0x01;
  return out;
}

describe("e2e 握手", () => {
  it("随机临时钥：两端得到同一组密钥与 SAS，SAS 是 6 位数字", async () => {
    const { identity, device, client } = await pair({ pinned: true });
    const core = await CoreHandshake.accept(client.m1, {
      sourceId: SOURCE_ID,
      identity,
    });
    await client.readCore(core.m2);
    const { m3, session } = await client.finish({
      device,
      platform: "desktop",
      name: "A",
    });
    const result = await core.readDevice(m3);
    expect(sessionHex(session)).toEqual(sessionHex(result.session));
    expect(session.sas).toMatch(/^\d{6}$/);
    expect(session.sas).toBe(result.session.sas);
    expect(session.devicePublicKey).toEqual(device.publicKey);
    expect(result.session.coreIdentity).toEqual(identity.publicKey);
  });

  it("中间人各自与两端握手：两侧 SAS 不同，钉扎的客户端认出身份不对", async () => {
    const real = await pair();
    const attacker = await ed25519SignerFromSeed(new Uint8Array(32).fill(9));
    // 客户端 ↔ 中间人（中间人冒充 core，只能用自己的身份钥）
    const toClient = await CoreHandshake.accept(real.client.m1, {
      sourceId: SOURCE_ID,
      identity: attacker,
    });
    const { coreIdentity } = await real.client.readCore(toClient.m2);
    expect(coreIdentity).toEqual(attacker.publicKey);
    const clientSide = await real.client.finish({
      device: real.device,
      platform: "browser",
      name: "B",
    });
    // 中间人 ↔ 真 core（中间人冒充客户端）
    const fake = await ClientHandshake.start({ sourceId: SOURCE_ID });
    const core = await CoreHandshake.accept(fake.m1, {
      sourceId: SOURCE_ID,
      identity: real.identity,
    });
    await fake.readCore(core.m2);
    const fakeSide = await fake.finish({
      device: attacker,
      platform: "browser",
      name: "B",
    });
    const coreSide = await core.readDevice(fakeSide.m3);
    expect(clientSide.session.sas).not.toBe(coreSide.session.sas);
    // 钉住了真 core 的客户端：中间人的 m2 过不了
    const pinned = await pair({ pinned: true });
    await expect(
      CoreHandshake.accept(pinned.client.m1, {
        sourceId: SOURCE_ID,
        identity: attacker,
      }),
    ).rejects.toEqual(e2eError("pin_mismatch"));
  });

  it("中间人把 m1 的 pinHint 抹成全零：转录不同，客户端解不开 m2", async () => {
    const { identity, client } = await pair({ pinned: true });
    const stripped = client.m1.slice();
    stripped.fill(0, M1_BYTES - 8);
    const core = await CoreHandshake.accept(stripped, {
      sourceId: SOURCE_ID,
      identity,
    });
    await expect(client.readCore(core.m2)).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
  });

  it("sourceId 不同（转录起点不同）：握手失败", async () => {
    const { identity, client } = await pair();
    const core = await CoreHandshake.accept(client.m1, {
      sourceId: "fedcba9876543210fedcba9876543210",
      identity,
    });
    await expect(client.readCore(core.m2)).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
  });

  it("篡改 m2 的任一部分：临时钥、密文、标签都让客户端失败", async () => {
    for (const index of [0, 31, 32, 100, 143]) {
      const { identity, client } = await pair();
      const core = await CoreHandshake.accept(client.m1, {
        sourceId: SOURCE_ID,
        identity,
      });
      await expect(
        client.readCore(flip(core.m2, index)),
        `字节 ${index}`,
      ).rejects.toBeInstanceOf(E2eError);
    }
  });

  it("篡改 m3：core 解不开", async () => {
    const { identity, device, client } = await pair();
    const core = await CoreHandshake.accept(client.m1, {
      sourceId: SOURCE_ID,
      identity,
    });
    await client.readCore(core.m2);
    const { m3 } = await client.finish({
      device,
      platform: "android",
      name: "C",
    });
    await expect(core.readDevice(flip(m3, 5))).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
  });

  it("签名不对：core 签了别的东西，客户端拒；设备签了别的东西，core 拒", async () => {
    const good = await ed25519SignerFromSeed(new Uint8Array(32).fill(7));
    const badSigner = (signer: E2eSigner): E2eSigner => ({
      publicKey: signer.publicKey,
      sign: (data) => signer.sign(flip(data, 0)),
    });
    {
      const { client } = await pair();
      const core = await CoreHandshake.accept(client.m1, {
        sourceId: SOURCE_ID,
        identity: badSigner(good),
      });
      await expect(client.readCore(core.m2)).rejects.toEqual(
        e2eError("signature_invalid"),
      );
    }
    {
      const { identity, device, client } = await pair();
      const core = await CoreHandshake.accept(client.m1, {
        sourceId: SOURCE_ID,
        identity,
      });
      await client.readCore(core.m2);
      const { m3 } = await client.finish({
        device: badSigner(device),
        platform: "ios",
        name: "D",
      });
      await expect(core.readDevice(m3)).rejects.toEqual(
        e2eError("signature_invalid"),
      );
    }
  });

  it("重放 m3 到另一次握手：转录不同，解不开", async () => {
    const first = await pair();
    const coreA = await CoreHandshake.accept(first.client.m1, {
      sourceId: SOURCE_ID,
      identity: first.identity,
    });
    await first.client.readCore(coreA.m2);
    const { m3 } = await first.client.finish({
      device: first.device,
      platform: "ios",
      name: "E",
    });
    const coreB = await CoreHandshake.accept(first.client.m1, {
      sourceId: SOURCE_ID,
      identity: first.identity,
    });
    await expect(coreB.readDevice(m3)).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
  });

  it("调用顺序：没读 m2 不能 finish，同一步不能做两次", async () => {
    const { identity, device, client } = await pair();
    await expect(
      client.finish({ device, platform: "ios", name: "F" }),
    ).rejects.toEqual(e2eError("state"));
    const core = await CoreHandshake.accept(client.m1, {
      sourceId: SOURCE_ID,
      identity,
    });
    await client.readCore(core.m2);
    await expect(client.readCore(core.m2)).rejects.toEqual(e2eError("state"));
  });

  it("小阶点：客户端临时钥全零，core 拒（weak_key）", async () => {
    const { identity, client } = await pair();
    const weak = client.m1.slice();
    weak.fill(0, 2, 34);
    await expect(
      CoreHandshake.accept(weak, { sourceId: SOURCE_ID, identity }),
    ).rejects.toEqual(e2eError("weak_key"));
  });
});

describe("e2e 消息布局", () => {
  it("m1：长度、版本、未定义的标志位", async () => {
    const { client } = await pair({ flags: E2E_FLAGS.padding });
    expect(decodeHello(client.m1).flags).toBe(E2E_FLAGS.padding);
    expect(() => decodeHello(client.m1.subarray(1))).toThrow(
      expect.objectContaining({ code: "bad_message" }),
    );
    const version = client.m1.slice();
    version[0] = 2;
    expect(() => decodeHello(version)).toThrow(
      expect.objectContaining({ code: "version_unsupported" }),
    );
    const flags = client.m1.slice();
    flags[1] = 0x82;
    expect(() => decodeHello(flags)).toThrow(
      expect.objectContaining({ code: "bad_message" }),
    );
  });

  it("m3 明文：多余字节、平台未知、proof 长度不对都拒", () => {
    const base = [
      ...new Uint8Array(32),
      ...new Uint8Array(64),
      3,
      ...new TextEncoder().encode("ios"),
      1,
      0x41,
      0,
    ];
    expect(decodeDevice(Uint8Array.from(base)).name).toBe("A");
    expect(() => decodeDevice(Uint8Array.from([...base, 0]))).toThrow(E2eError);
    const platform = base.slice();
    platform.splice(97, 3, ...new TextEncoder().encode("tv1"));
    expect(() => decodeDevice(Uint8Array.from(platform))).toThrow(E2eError);
    const proof = base.slice();
    proof[proof.length - 1] = 5;
    expect(() => decodeDevice(Uint8Array.from(proof))).toThrow(E2eError);
  });
});

// ------------------------------------------------------------------ 记录层

async function layers(flags = 0) {
  const { identity, device, client } = await pair({ flags });
  const core = await CoreHandshake.accept(client.m1, {
    sourceId: SOURCE_ID,
    identity,
  });
  await client.readCore(core.m2);
  const finished = await client.finish({
    device,
    platform: "desktop",
    name: "G",
  });
  const result = await core.readDevice(finished.m3);
  return {
    client: await createRecordLayer(finished.session, "client"),
    core: await createRecordLayer(result.session, "core"),
  };
}

const ping: Frame = { type: "ping", ts: 1 };

describe("e2e 记录层", () => {
  it("双向往返：每个方向计数器各自递增", async () => {
    const { client, core } = await layers();
    for (let index = 0; index < 3; index += 1) {
      const up = await client.sealer.sealFrames([ping]);
      expect(await core.opener.openFrames(up)).toEqual([ping]);
    }
    const down = await core.sealer.sealFrames([{ type: "pong", ts: 1 }]);
    expect(await client.opener.openFrames(down)).toEqual([
      { type: "pong", ts: 1 },
    ]);
    expect(client.sealer.records).toBe(3);
    expect(core.sealer.records).toBe(1);
  });

  it("重放同一条记录：第二次失败，之后这个方向作废", async () => {
    const { client, core } = await layers();
    const unit = await client.sealer.sealFrames([ping]);
    await core.opener.open(unit);
    await expect(core.opener.open(unit)).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
    const next = await client.sealer.sealFrames([ping]);
    await expect(core.opener.open(next)).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
  });

  it("乱序：先到的第二条记录打不开", async () => {
    const { client, core } = await layers();
    await client.sealer.sealFrames([ping]);
    const second = await client.sealer.sealFrames([ping]);
    await expect(core.opener.open(second)).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
  });

  it("方向不能互换：自己发的记录自己打不开", async () => {
    const { client } = await layers();
    const unit = await client.sealer.sealFrames([ping]);
    await expect(client.opener.open(unit)).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
  });

  it("篡改密文、标签、长度字段都失败", async () => {
    for (const index of [4, 10, -1]) {
      const { client, core } = await layers();
      const unit = await client.sealer.sealFrames([ping]);
      const at = index < 0 ? unit.length + index : index;
      await expect(
        core.opener.open(flip(unit, at)),
        `字节 ${at}`,
      ).rejects.toEqual(e2eError("decrypt_failed"));
    }
    const { client, core } = await layers();
    const unit = await client.sealer.sealFrames([ping]);
    await expect(core.opener.open(flip(unit, 3))).rejects.toEqual(
      e2eError("record_too_large"),
    );
    await expect(core.opener.open(unit)).rejects.toEqual(
      e2eError("decrypt_failed"),
    );
  });

  it("截断：长度对不上直接拒", async () => {
    const { client, core } = await layers();
    const unit = await client.sealer.sealFrames([ping]);
    await expect(
      core.opener.open(unit.subarray(0, unit.length - 1)),
    ).rejects.toBeInstanceOf(E2eError);
  });

  it("上限：64 KiB 明文刚好装下，多 1 字节拒；内层 DATA 有自己的上限", async () => {
    const full = new Uint8Array(LIMITS_E2E.maxRecordEntries - 4);
    expect(packEntries([full], false)).toHaveLength(
      LIMITS_E2E.maxRecordEntries,
    );
    expect(() => packEntries([new Uint8Array(full.length + 1)], false)).toThrow(
      expect.objectContaining({ code: "record_too_large" }),
    );
    const { client, core } = await layers();
    const data: Frame = {
      type: "data",
      streamId: 1,
      bytes: new Uint8Array(LIMITS_E2E.maxInnerDataChunk).fill(3),
    };
    const unit = await client.sealer.sealFrames([data]);
    expect(unit.length).toBeLessThanOrEqual(4 + LIMITS_E2E.maxRecordCiphertext);
    expect(await core.opener.openFrames(unit)).toEqual([data]);
    expect(() =>
      client.sealer.sealFrames([
        { ...data, bytes: new Uint8Array(LIMITS_E2E.maxInnerDataChunk + 1) },
      ]),
    ).toThrow(expect.objectContaining({ code: "record_too_large" }));
    const oversized = new Uint8Array(4 + LIMITS_E2E.maxRecordCiphertext + 1);
    new DataView(oversized.buffer).setUint32(
      0,
      LIMITS_E2E.maxRecordCiphertext + 1,
    );
    await expect(core.opener.open(oversized)).rejects.toEqual(
      e2eError("record_too_large"),
    );
  });

  it("计数器到 2³² 抛 record_limit", async () => {
    const { client } = await layers();
    Object.assign(client.sealer, {
      counter: LIMITS_E2E.maxRecordsPerDirection,
    });
    expect(() => client.sealer.sealFrames([ping])).toThrow(
      expect.objectContaining({ code: "record_limit" }),
    );
  });

  it("并发 seal：结果按调用顺序对应计数器", async () => {
    const { client, core } = await layers();
    const frames = Array.from(
      { length: 20 },
      (_, ts): Frame => ({ type: "ping", ts }),
    );
    const units = await Promise.all(
      frames.map((frame) => client.sealer.sealFrames([frame])),
    );
    for (const [index, unit] of units.entries())
      expect(await core.opener.openFrames(unit)).toEqual([frames[index]]);
  });

  it("填充：开了就补到 64 的倍数，对端去掉；没协商的填充标记拒", async () => {
    const { client, core } = await layers(E2E_FLAGS.padding);
    const unit = await client.sealer.sealFrames([ping]);
    expect((unit.length - 4 - 16) % 64).toBe(0);
    expect(await core.opener.openFrames(unit)).toEqual([ping]);
    const padded = packEntries([new Uint8Array([1, 2, 3])], true);
    expect(padded).toHaveLength(64);
    expect(unpackEntries(padded, true)).toEqual([new Uint8Array([1, 2, 3])]);
    expect(() => unpackEntries(padded, false)).toThrow(
      expect.objectContaining({ code: "bad_record" }),
    );
    const dirty = padded.slice();
    dirty[63] = 1;
    expect(() => unpackEntries(dirty, true)).toThrow(
      expect.objectContaining({ code: "bad_record" }),
    );
    expect(packEntries([new Uint8Array(60)], true)).toHaveLength(64);
  });

  it("条目结构：空条目、越界、截断的长度都拒", () => {
    expect(() => packEntries([new Uint8Array(0)], false)).toThrow(E2eError);
    expect(() => unpackEntries(Uint8Array.of(0, 0, 0, 9, 1), false)).toThrow(
      E2eError,
    );
    expect(() => unpackEntries(Uint8Array.of(0, 0), false)).toThrow(E2eError);
  });

  it("accept：恰好一个 JSON 条目，schema 校验", () => {
    const accept: E2eAccept = {
      type: "accept",
      deviceStatus: "pending",
      pendingId: "pnd_1",
      limits: {
        maxStreams: 1,
        streamWindow: 1,
        tunnelWindow: 1,
        maxFrameBytes: 1,
      },
    };
    const entry = encodeAccept(accept);
    expect(decodeAccept([entry])).toEqual(accept);
    expect(() => decodeAccept([entry, entry])).toThrow(E2eError);
    expect(() =>
      decodeAccept([
        new TextEncoder().encode('{"type":"accept","deviceStatus":"x"}'),
      ]),
    ).toThrow(E2eError);
  });
});

describe("e2e 单元切分", () => {
  it("任意切分都能还原：逐字节喂与整块喂结果相同", async () => {
    const { client } = await layers();
    const units = [
      encodeUnit(new Uint8Array(M1_BYTES).fill(1)),
      await client.sealer.sealFrames([ping]),
      await client.sealer.sealFrames([
        { type: "data", streamId: 3, bytes: new Uint8Array(300) },
      ]),
    ];
    const stream = Uint8Array.from(units.flatMap((unit) => [...unit]));
    const whole = new UnitReader().feed(stream);
    expect(whole).toEqual(units);
    const reader = new UnitReader();
    const pieces: Uint8Array[] = [];
    for (const byte of stream) pieces.push(...reader.feed(Uint8Array.of(byte)));
    expect(pieces).toEqual(units);
    expect(reader.pending).toBe(0);
    expect(unitBody(units[0] as Uint8Array)).toHaveLength(M1_BYTES);
  });

  it("长度超限立即失败，不等字节收全", () => {
    const reader = new UnitReader(LIMITS_E2E.maxHandshakeMessageBytes);
    expect(() => reader.feed(Uint8Array.of(0, 0, 0x04, 0x01))).toThrow(
      expect.objectContaining({ code: "record_too_large" }),
    );
    expect(() =>
      encodeUnit(new Uint8Array(LIMITS_E2E.maxRecordCiphertext + 1)),
    ).toThrow(E2eError);
  });
});
