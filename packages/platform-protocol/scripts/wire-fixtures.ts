/**
 * 线上字节的黄金夹具：`fixtures/http1/`（隧道流里 core 写回的 HTTP/1.1 响应、中继写进去的
 * 请求）与 `fixtures/ws/`（WebSocket 帧，含 RFC 6455 §5.7 的示例）。字节在这里逐个写死，
 * 不经任何实现生成；期望结果写在同目录的 `cases.json`，各语言的实现对着它测。
 *
 * `cases.json` 里体与载荷的写法：`text`（UTF-8）、`hex`，或 `{ length, fill }`（`length`
 * 个字节都是 `fill`）。
 */
const encoder = new TextEncoder();
const text = (value: string) => encoder.encode(value);
const hex = (value: string) =>
  Uint8Array.from(value.match(/../g) ?? [], (pair) =>
    Number.parseInt(pair, 16),
  );
const toHex = (bytes: Uint8Array) =>
  [...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("");
const concat = (...parts: Uint8Array[]) => {
  const out = new Uint8Array(parts.reduce((sum, part) => sum + part.length, 0));
  let at = 0;
  for (const part of parts) {
    out.set(part, at);
    at += part.length;
  }
  return out;
};
const filled = (length: number, fill: number) =>
  new Uint8Array(length).fill(fill);

type Bytes =
  | { text: string }
  | { hex: string }
  | { length: number; fill: number };

// ------------------------------------------------------------------ http1

interface ResponseCase {
  name: string;
  file: string;
  /** 请求方法（`HEAD` 的响应没有体）。 */
  method: string;
  expect?: {
    version: string;
    status: number;
    statusText: string;
    headers: [string, string][];
    body: Bytes;
    /** `101`：之后的字节原样交出（`upgraded`）。 */
    upgrade: boolean;
    upgraded?: Bytes;
    /** 关闭定界：要等流结束才算完整。 */
    needsEof: boolean;
  };
  /** 不合法：解析器报的原因（本仓实现的分类，其他实现只要求报错）。 */
  error?: string;
  /** `error` 在流结束时才报（体没收全）。 */
  errorAtEof?: boolean;
}

interface RequestCase {
  name: string;
  file: string;
  method: string;
  path: string;
  headers: [string, string][];
  framing: "none" | "length" | "chunked";
  /** 体按这些块写（`chunked` 时每块各包一层）。 */
  chunks: string[];
}

const RESPONSES: { case: ResponseCase; bytes: Uint8Array }[] = [];
const REQUESTS: { case: RequestCase; bytes: Uint8Array }[] = [];

function response(entry: Omit<ResponseCase, "file">, bytes: Uint8Array) {
  RESPONSES.push({
    case: { ...entry, file: `http1/response-${entry.name}.bin` },
    bytes,
  });
}

response(
  {
    name: "content-length",
    method: "GET",
    expect: {
      version: "1.1",
      status: 200,
      statusText: "OK",
      headers: [
        ["Content-Type", "text/plain; charset=utf-8"],
        ["Content-Length", "5"],
        ["Set-Cookie", "a=1"],
        ["set-cookie", "b=2"],
      ],
      body: { text: "hello" },
      upgrade: false,
      needsEof: false,
    },
  },
  text(
    "HTTP/1.1 200 OK\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: 5\r\n" +
      "Set-Cookie: a=1\r\nset-cookie: b=2\r\n\r\nhello",
  ),
);
response(
  {
    name: "chunked",
    method: "GET",
    expect: {
      version: "1.1",
      status: 200,
      statusText: "OK",
      headers: [
        ["content-type", "application/json"],
        ["transfer-encoding", "chunked"],
      ],
      body: { text: '{"ok":true,"items":[]}' },
      upgrade: false,
      needsEof: false,
    },
  },
  text(
    "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ntransfer-encoding: chunked\r\n\r\n" +
      'a;ext=1\r\n{"ok":true\r\n' +
      'C\r\n,"items":[]}\r\n' +
      "0\r\nx-trailer: t\r\n\r\n",
  ),
);
response(
  {
    name: "close-delimited",
    method: "GET",
    expect: {
      version: "1.0",
      status: 200,
      statusText: "OK",
      headers: [["content-type", "text/event-stream"]],
      body: { text: "data: 1\n\ndata: 2\n\n" },
      upgrade: false,
      needsEof: true,
    },
  },
  text(
    "HTTP/1.0 200 OK\r\ncontent-type: text/event-stream\r\n\r\ndata: 1\n\ndata: 2\n\n",
  ),
);
response(
  {
    name: "head",
    method: "HEAD",
    expect: {
      version: "1.1",
      status: 200,
      statusText: "OK",
      headers: [["content-length", "1048576"]],
      body: { text: "" },
      upgrade: false,
      needsEof: false,
    },
  },
  text("HTTP/1.1 200 OK\r\ncontent-length: 1048576\r\n\r\n"),
);
response(
  {
    name: "no-content",
    method: "DELETE",
    expect: {
      version: "1.1",
      status: 204,
      statusText: "No Content",
      headers: [["x-request-id", "r1"]],
      body: { text: "" },
      upgrade: false,
      needsEof: false,
    },
  },
  text("HTTP/1.1 204 No Content\r\nx-request-id: r1\r\n\r\n"),
);
response(
  {
    name: "not-modified",
    method: "GET",
    expect: {
      version: "1.1",
      status: 304,
      statusText: "Not Modified",
      headers: [
        ["etag", '"v1"'],
        ["content-length", "42"],
      ],
      body: { text: "" },
      upgrade: false,
      needsEof: false,
    },
  },
  text('HTTP/1.1 304 Not Modified\r\netag: "v1"\r\ncontent-length: 42\r\n\r\n'),
);
response(
  {
    name: "continue-then-ok",
    method: "POST",
    expect: {
      version: "1.1",
      status: 201,
      statusText: "Created",
      headers: [["content-length", "2"]],
      body: { text: "{}" },
      upgrade: false,
      needsEof: false,
    },
  },
  text(
    "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 201 Created\r\ncontent-length: 2\r\n\r\n{}",
  ),
);
response(
  {
    name: "switching-protocols",
    method: "GET",
    expect: {
      version: "1.1",
      status: 101,
      statusText: "Switching Protocols",
      headers: [
        ["Upgrade", "websocket"],
        ["Connection", "Upgrade"],
        ["Sec-WebSocket-Accept", "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="],
        ["Sec-WebSocket-Protocol", "armadra-ticket.test-ticket"],
      ],
      body: { text: "" },
      upgrade: true,
      upgraded: { hex: "810548656c6c6f" },
      needsEof: false,
    },
  },
  concat(
    text(
      "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
        "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
        "Sec-WebSocket-Protocol: armadra-ticket.test-ticket\r\n\r\n",
    ),
    hex("810548656c6c6f"),
  ),
);
response(
  {
    name: "empty-reason-latin1",
    method: "GET",
    expect: {
      version: "1.1",
      status: 200,
      statusText: "",
      headers: [
        ["content-disposition", "attachment; filename=été.txt"],
        ["content-length", "0"],
      ],
      body: { text: "" },
      upgrade: false,
      needsEof: false,
    },
  },
  concat(
    text("HTTP/1.1 200\r\ncontent-disposition: attachment; filename="),
    hex("e974e9"),
    text(".txt\r\ncontent-length: 0\r\n\r\n"),
  ),
);
response(
  {
    name: "large-body",
    method: "GET",
    expect: {
      version: "1.1",
      status: 206,
      statusText: "Partial Content",
      headers: [
        ["content-range", "bytes 0-65535/1000000"],
        ["content-length", "65536"],
      ],
      body: { length: 65_536, fill: 7 },
      upgrade: false,
      needsEof: false,
    },
  },
  concat(
    text(
      "HTTP/1.1 206 Partial Content\r\ncontent-range: bytes 0-65535/1000000\r\ncontent-length: 65536\r\n\r\n",
    ),
    filled(65_536, 7),
  ),
);

const invalid: [string, string, Uint8Array, boolean?][] = [
  ["bad-status-line", "bad_status_line", text("HTTP/1.1 abc OK\r\n\r\n")],
  ["bad-version", "bad_status_line", text("HTTP/2 200 OK\r\n\r\n")],
  [
    "space-before-colon",
    "bad_header",
    text("HTTP/1.1 200 OK\r\ncontent-length : 0\r\n\r\n"),
  ],
  [
    "obs-fold",
    "bad_header",
    text(
      "HTTP/1.1 200 OK\r\nx-a: 1\r\n continued\r\ncontent-length: 0\r\n\r\n",
    ),
  ],
  [
    "length-and-chunked",
    "bad_transfer_encoding",
    text(
      "HTTP/1.1 200 OK\r\ncontent-length: 3\r\ntransfer-encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n",
    ),
  ],
  [
    "conflicting-lengths",
    "bad_content_length",
    text("HTTP/1.1 200 OK\r\ncontent-length: 5, 6\r\n\r\nhello"),
  ],
  [
    "negative-length",
    "bad_content_length",
    text("HTTP/1.1 200 OK\r\ncontent-length: -1\r\n\r\n"),
  ],
  [
    "bad-chunk-size",
    "bad_chunk",
    text(
      "HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\nzz\r\nabc\r\n0\r\n\r\n",
    ),
  ],
  [
    "chunk-missing-crlf",
    "bad_chunk",
    text(
      "HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n3\r\nabcX\r\n0\r\n\r\n",
    ),
  ],
  [
    "head-too-large",
    "head_too_large",
    text(`HTTP/1.1 200 OK\r\nx-big: ${"a".repeat(32_768)}\r\n\r\n`),
  ],
  [
    "truncated-length",
    "truncated",
    text("HTTP/1.1 200 OK\r\ncontent-length: 10\r\n\r\nhello"),
    true,
  ],
  [
    "truncated-chunked",
    "truncated",
    text("HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\n\r\n5\r\nhel"),
    true,
  ],
  ["truncated-head", "truncated", text("HTTP/1.1 200 OK\r\ncontent-le"), true],
];
for (const [name, error, bytes, atEof] of invalid)
  response(
    { name, method: "GET", error, ...(atEof ? { errorAtEof: true } : {}) },
    bytes,
  );

function request(entry: Omit<RequestCase, "file">, bytes: Uint8Array) {
  REQUESTS.push({
    case: { ...entry, file: `http1/request-${entry.name}.bin` },
    bytes,
  });
}

request(
  {
    name: "get",
    method: "GET",
    path: "/api/workspaces?limit=10",
    headers: [
      ["host", "relay.armadra.test"],
      ["x-forwarded-for", "203.0.113.7"],
      ["x-forwarded-proto", "https"],
      ["connection", "close"],
    ],
    framing: "none",
    chunks: [],
  },
  text(
    "GET /api/workspaces?limit=10 HTTP/1.1\r\nhost: relay.armadra.test\r\n" +
      "x-forwarded-for: 203.0.113.7\r\nx-forwarded-proto: https\r\nconnection: close\r\n\r\n",
  ),
);
request(
  {
    name: "post-length",
    method: "POST",
    path: "/api/echo",
    headers: [
      ["host", "relay.armadra.test"],
      ["content-type", "application/json"],
      ["content-length", "13"],
      ["connection", "close"],
    ],
    framing: "length",
    chunks: ['{"a":', "1234567}"],
  },
  text(
    "POST /api/echo HTTP/1.1\r\nhost: relay.armadra.test\r\ncontent-type: application/json\r\n" +
      'content-length: 13\r\nconnection: close\r\n\r\n{"a":1234567}',
  ),
);
request(
  {
    name: "post-chunked",
    method: "POST",
    path: "/api/upload",
    headers: [
      ["host", "relay.armadra.test"],
      ["connection", "close"],
    ],
    framing: "chunked",
    chunks: ["hello, ", "world! this chunk is longer"],
  },
  text(
    "POST /api/upload HTTP/1.1\r\nhost: relay.armadra.test\r\nconnection: close\r\n" +
      "transfer-encoding: chunked\r\n\r\n7\r\nhello, \r\n1b\r\nworld! this chunk is longer\r\n0\r\n\r\n",
  ),
);

// ------------------------------------------------------------------ ws

interface WsCase {
  name: string;
  file: string;
  /** 发送方：`client` 的帧必须带掩码，`server` 的不得带（RFC 6455 §5.1）。 */
  from: "client" | "server";
  events?: (
    | { type: "message"; binary: boolean; data: Bytes }
    | { type: "close"; code?: number; reason: string }
    | { type: "ping" | "pong"; data: Bytes }
  )[];
  /** 依次编这些帧，拼起来等于文件字节。 */
  encode?: {
    opcode: number;
    fin: boolean;
    payload: Bytes;
    mask?: string;
  }[];
  error?: string;
}

const WS: { case: WsCase; bytes: Uint8Array }[] = [];
function ws(entry: Omit<WsCase, "file">, bytes: Uint8Array) {
  WS.push({ case: { ...entry, file: `ws/${entry.name}.bin` }, bytes });
}

const MASK = "37fa213d";
ws(
  {
    name: "rfc-text-unmasked",
    from: "server",
    events: [{ type: "message", binary: false, data: { text: "Hello" } }],
    encode: [{ opcode: 1, fin: true, payload: { text: "Hello" } }],
  },
  hex("810548656c6c6f"),
);
ws(
  {
    name: "rfc-text-masked",
    from: "client",
    events: [{ type: "message", binary: false, data: { text: "Hello" } }],
    encode: [{ opcode: 1, fin: true, payload: { text: "Hello" }, mask: MASK }],
  },
  hex("818537fa213d7f9f4d5158"),
);
ws(
  {
    name: "rfc-fragmented",
    from: "server",
    events: [{ type: "message", binary: false, data: { text: "Hello" } }],
    encode: [
      { opcode: 1, fin: false, payload: { text: "Hel" } },
      { opcode: 0, fin: true, payload: { text: "lo" } },
    ],
  },
  hex("010348656c80026c6f"),
);
ws(
  {
    name: "fragmented-with-ping",
    from: "client",
    events: [
      { type: "ping", data: { text: "" } },
      { type: "message", binary: true, data: { hex: "0102030405" } },
    ],
    encode: [
      { opcode: 2, fin: false, payload: { hex: "0102" }, mask: MASK },
      { opcode: 9, fin: true, payload: { hex: "" }, mask: MASK },
      { opcode: 0, fin: true, payload: { hex: "030405" }, mask: MASK },
    ],
  },
  hex("0282" + MASK + "36f8" + "8980" + MASK + "8083" + MASK + "34fe24"),
);
ws(
  {
    name: "rfc-ping",
    from: "server",
    events: [{ type: "ping", data: { text: "Hello" } }],
    encode: [{ opcode: 9, fin: true, payload: { text: "Hello" } }],
  },
  hex("890548656c6c6f"),
);
ws(
  {
    name: "rfc-pong-masked",
    from: "client",
    events: [{ type: "pong", data: { text: "Hello" } }],
    encode: [{ opcode: 10, fin: true, payload: { text: "Hello" }, mask: MASK }],
  },
  hex("8a8537fa213d7f9f4d5158"),
);
ws(
  {
    name: "rfc-binary-256",
    from: "server",
    events: [
      { type: "message", binary: true, data: { length: 256, fill: 0x2a } },
    ],
    encode: [{ opcode: 2, fin: true, payload: { length: 256, fill: 0x2a } }],
  },
  concat(hex("827e0100"), filled(256, 0x2a)),
);
ws(
  {
    name: "rfc-binary-64k",
    from: "server",
    events: [
      { type: "message", binary: true, data: { length: 65_536, fill: 7 } },
    ],
    encode: [{ opcode: 2, fin: true, payload: { length: 65_536, fill: 7 } }],
  },
  concat(hex("827f0000000000010000"), filled(65_536, 7)),
);
ws(
  {
    name: "close-source-offline",
    from: "server",
    events: [{ type: "close", code: 4404, reason: "source_offline" }],
    encode: [
      {
        opcode: 8,
        fin: true,
        payload: { hex: "1134" + toHex(text("source_offline")) },
      },
    ],
  },
  concat(hex("88101134"), text("source_offline")),
);
ws(
  {
    name: "close-empty",
    from: "client",
    events: [{ type: "close", reason: "" }],
    encode: [{ opcode: 8, fin: true, payload: { hex: "" }, mask: MASK }],
  },
  hex("8880" + MASK),
);
const wsInvalid: [string, WsCase["from"], string, string][] = [
  ["reserved-bit", "server", "reserved_bits", "c10548656c6c6f"],
  ["unknown-opcode", "server", "bad_opcode", "8300"],
  ["fragmented-ping", "server", "bad_control_frame", "0900"],
  ["long-ping", "server", "bad_control_frame", "897e007e" + "00".repeat(126)],
  ["orphan-continuation", "server", "bad_continuation", "800161"],
  ["interleaved-data", "server", "bad_continuation", "010161810161"],
  ["unmasked-from-client", "client", "mask_required", "810548656c6c6f"],
  ["masked-from-server", "server", "mask_forbidden", "818537fa213d7f9f4d5158"],
  ["close-one-byte", "server", "bad_close_payload", "880111"],
];
for (const [name, from, error, bytes] of wsInvalid)
  ws({ name, from, error }, hex(bytes));

// ------------------------------------------------------------------ out

const json = (value: unknown) => text(`${JSON.stringify(value, null, 2)}\n`);

/** 全部线上字节夹具：相对 `fixtures/` 的名字 → 字节。 */
export function wireFixtures(): Map<string, Uint8Array> {
  const files = new Map<string, Uint8Array>();
  for (const entry of [...RESPONSES, ...REQUESTS, ...WS])
    files.set(entry.case.file, entry.bytes);
  files.set(
    "http1/cases.json",
    json({
      $comment:
        "HTTP/1.1 线上字节（隧道流里 core 写回的响应、中继写进去的请求）。体写成 text（UTF-8）、hex 或 { length, fill }；error 是本仓实现的分类，其他实现只要求报错。",
      responses: RESPONSES.map((entry) => entry.case),
      requests: REQUESTS.map((entry) => entry.case),
    }),
  );
  files.set(
    "ws/cases.json",
    json({
      $comment:
        "WebSocket 帧（RFC 6455 §5；rfc-* 是 §5.7 的示例）。from=client 的帧必须带掩码、server 的不得带；encode 依次编码后拼起来等于文件字节。",
      frames: WS.map((entry) => entry.case),
    }),
  );
  return files;
}
