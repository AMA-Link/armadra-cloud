// 在服务器壳容器里跑的管理员小工具：脚本把它连同参数从标准输入交给 `node -`。
// 服务器壳不发布端口，管理员的调用（配对、登记、邀请）只能从容器里发给它自己。
//
// 前面由 e2e.mjs 拼上一行 `const input = {...};`：
//   { action: "pair", origin, ticket }                 配对，会话（Cookie 与 CSRF）存进 /tmp
//   { action: "call", origin, method, path, body? }    带会话调用，答 { status, body }
// 只在标准输出打一行 JSON；会话文件 0600，只在容器的临时目录里，容器随项目一起删。
/* global input -- 由 e2e.mjs 在前面拼上 */
import { readFileSync, writeFileSync } from "node:fs";
import { request } from "node:https";

const SESSION = "/tmp/e2e-admin-session.json";
const url = new URL(input.origin);

function send(method, path, headers, body) {
  const payload = body === undefined ? undefined : JSON.stringify(body);
  return new Promise((resolve, reject) => {
    const outgoing = request(
      {
        host: "127.0.0.1",
        port: url.port || 443,
        servername: url.hostname,
        method,
        path,
        rejectUnauthorized: false, // 容器回环上的自签证书，只对自己说话
        agent: false,
        headers: {
          host: url.host,
          origin: input.origin,
          ...(payload === undefined
            ? {}
            : {
                "content-type": "application/json",
                "content-length": String(Buffer.byteLength(payload)),
              }),
          ...headers,
        },
      },
      (response) => {
        const chunks = [];
        response.on("data", (chunk) => chunks.push(chunk));
        response.on("end", () => {
          const text = Buffer.concat(chunks).toString("utf8");
          let parsed = {};
          try {
            parsed = text === "" ? {} : JSON.parse(text);
          } catch {
            parsed = { raw: text.slice(0, 200) };
          }
          resolve({
            status: response.statusCode ?? 0,
            body: parsed,
            cookies: (response.headers["set-cookie"] ?? []).map(
              (line) => line.split(";")[0],
            ),
          });
        });
      },
    );
    outgoing.on("error", reject);
    if (payload !== undefined) outgoing.write(payload);
    outgoing.end();
  });
}

let result;
if (input.action === "pair") {
  const paired = await send(
    "POST",
    "/api/identity/pair",
    {},
    {
      ticket: input.ticket,
    },
  );
  const csrf = paired.body.csrfToken ?? paired.body.session?.csrfToken ?? "";
  if (paired.status === 200) {
    writeFileSync(
      SESSION,
      JSON.stringify({ cookie: paired.cookies.join("; "), csrf }),
      { mode: 0o600 },
    );
  }
  result = { status: paired.status, body: { code: paired.body.code ?? null } };
} else {
  const session = JSON.parse(readFileSync(SESSION, "utf8"));
  const answer = await send(
    input.method,
    input.path,
    { cookie: session.cookie, "x-armadra-csrf": session.csrf },
    input.body,
  );
  result = { status: answer.status, body: answer.body };
}
process.stdout.write(`${JSON.stringify(result)}\n`);
