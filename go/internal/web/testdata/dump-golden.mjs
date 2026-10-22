// 生成 *.golden：把 TS 版 renderPage 的输出（HTML 与 CSP）落成 Go 测试的对照数据。
// 重新生成（在仓库根目录，Node 24）：
//   node go/internal/web/testdata/dump-golden.mjs
// 随机 nonce 统一换成 NONCE。文件格式：第一段 CSP，分隔行 "=====" 之后是 HTML。
// 这个脚本不由 go test 运行；改了 packages/cloud-shared/src/pages.ts 的页面后要重跑并提交。
import { writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { renderPage } from "../../../../packages/cloud-shared/src/pages.ts";

const dir = dirname(fileURLToPath(import.meta.url));
const LINK = "0123456789abcdef";

for (const locale of ["zh-CN", "en"]) {
  for (const kind of ["unavailable", "join"]) {
    const { html, csp } = renderPage(
      locale,
      kind,
      kind === "join" ? LINK : undefined,
    );
    const nonce = /'nonce-([^']+)'/.exec(csp)[1];
    const norm = (text) => text.split(nonce).join("NONCE");
    writeFileSync(
      join(dir, `${locale}_${kind}.golden`),
      `${norm(csp)}\n=====\n${norm(html)}`,
    );
  }
}
