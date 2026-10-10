/** 线上字节夹具（`fixtures/http1/`、`fixtures/ws/`）里体与载荷的三种写法。 */
export type FixtureBytes =
  | { text: string }
  | { hex: string }
  | { length: number; fill: number };

export function fixtureBytes(value: FixtureBytes): Uint8Array {
  if ("text" in value) return new TextEncoder().encode(value.text);
  if ("hex" in value)
    return Uint8Array.from(value.hex.match(/../g) ?? [], (pair) =>
      Number.parseInt(pair, 16),
    );
  return new Uint8Array(value.length).fill(value.fill);
}

export function joinBytes(parts: readonly Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((sum, part) => sum + part.length, 0));
  let at = 0;
  for (const part of parts) {
    out.set(part, at);
    at += part.length;
  }
  return out;
}
