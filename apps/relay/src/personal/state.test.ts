import {
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import { PASSWORD } from "../../test/personal.ts";
import { createState } from "./init.ts";
import { STATE_FILE, StateError, StateStore, type State } from "./state.ts";

const root = mkdtempSync(join(tmpdir(), "relay-state-"));
afterAll(() => rmSync(root, { recursive: true, force: true }));

let template: State;
beforeAll(async () => {
  template = (
    await createState({
      dataDir: join(root, "template"),
      account: "me",
      password: PASSWORD,
      issuer: "https://relay.test",
    })
  ).current;
});

function fresh(name: string): StateStore {
  return StateStore.create(join(root, name), structuredClone(template));
}

describe("状态文件", () => {
  it("0600、原子写（不留临时文件）、只存哈希不存口令", () => {
    const store = fresh("a");
    const file = join(store.dataDir, STATE_FILE);
    if (process.platform !== "win32")
      expect(statSync(file).mode & 0o777).toBe(0o600);
    store.update((draft) => {
      draft.settings.allowGuests = false;
    });
    expect(store.current.revision).toBe(template.revision + 1);
    expect(
      readdirSync(store.dataDir).filter((name) => name.endsWith(".tmp")),
    ).toEqual([]);
    const text = readFileSync(file, "utf8");
    expect(text).not.toContain(PASSWORD);
    expect(JSON.parse(text).account.password.kdf).toBe("scrypt");
  });

  it("已存在不覆盖；不存在答 not_initialized", () => {
    fresh("b");
    expect(() => StateStore.create(join(root, "b"), template)).toThrow(
      StateError,
    );
    expect(() => StateStore.open(join(root, "nope"))).toThrow(
      expect.objectContaining({ code: "not_initialized" }),
    );
  });

  it("坏 JSON、未知版本、字段不符一律拒绝，不自动重建", () => {
    const dir = join(root, "c");
    fresh("c");
    const file = join(dir, STATE_FILE);
    for (const text of [
      "{not json",
      JSON.stringify({ ...template, version: 2 }),
      JSON.stringify({ ...template, signingKeys: [] }),
    ]) {
      writeFileSync(file, text);
      expect(() => StateStore.open(dir)).toThrow(
        expect.objectContaining({ code: "invalid" }),
      );
      expect(readFileSync(file, "utf8")).toBe(text);
    }
  });

  it("两个进程（两个 store）交替写：后写的先合入别人的修订，谁的改动都不丢", () => {
    const first = fresh("d");
    const second = StateStore.open(first.dataDir);
    first.update((draft) => {
      draft.settings.trustedOrigins.push("https://a.example");
    });
    second.update((draft) => {
      draft.settings.trustedOrigins.push("https://b.example");
    });
    expect(
      StateStore.open(first.dataDir).current.settings.trustedOrigins,
    ).toEqual(["https://a.example", "https://b.example"]);
    expect(first.reload()).toBe(true);
    expect(first.current.settings.trustedOrigins).toHaveLength(2);
  });

  it("fn 抛错或什么也没改时不写盘", () => {
    const store = fresh("e");
    const before = store.current.revision;
    expect(() =>
      store.update(() => {
        throw new Error("no");
      }),
    ).toThrow("no");
    store.update(() => undefined);
    expect(StateStore.open(store.dataDir).current.revision).toBe(before);
  });
});
