/**
 * docs/contracts/cloud-api.md 的生成块。
 *
 *   node tools/contract/generate.mjs           重写生成块
 *   node tools/contract/generate.mjs --check   生成块与契约不一致就失败
 *
 * 生成块形如 `<!-- rpc:begin contract=§2 -->` … `<!-- rpc:end -->`。C0-1 时协议包
 * 还没有 `/v1` 契约，块里是占位；C0-2 起由 `@armadra/platform-protocol/cloud-api`
 * 生成形状表与 docs/contracts/cloud-openapi.json。
 */
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../", import.meta.url));
export const CONTRACT_FILE = "docs/contracts/cloud-api.md";

const BLOCK = /<!-- rpc:begin contract=(§\d+) -->\n([\s\S]*?)<!-- rpc:end -->/g;

export const PLACEHOLDER = "_形状表由协议包的 `/v1` 契约生成（C0-2）。_\n";

/** 找出全部生成块；begin / end 不配对时抛错。 */
export function findBlocks(markdown) {
  const begins = markdown.match(/<!-- rpc:begin /g)?.length ?? 0;
  const ends = markdown.match(/<!-- rpc:end -->/g)?.length ?? 0;
  if (begins !== ends)
    throw new Error(`生成块不配对：${begins} 个 begin，${ends} 个 end`);
  return [...markdown.matchAll(BLOCK)].map((match) => ({
    section: match[1],
    body: match[2],
  }));
}

/** 用 `render(section)` 重写每个生成块；前后各留一个空行，与 Prettier 的输出一致。 */
export function fill(markdown, render) {
  findBlocks(markdown);
  return markdown.replace(
    BLOCK,
    (_, section) =>
      `<!-- rpc:begin contract=${section} -->\n\n${render(section)}\n<!-- rpc:end -->`,
  );
}

function main(argv) {
  const path = join(root, CONTRACT_FILE);
  if (!existsSync(path)) {
    console.error(`契约文件缺失：${CONTRACT_FILE}`);
    return 1;
  }
  const current = readFileSync(path, "utf8");
  const next = fill(current, () => PLACEHOLDER);
  if (argv.includes("--check")) {
    if (next !== current) {
      console.error(
        `${CONTRACT_FILE} 的生成块过期；运行 pnpm contract:generate。`,
      );
      return 1;
    }
    console.log(
      `${CONTRACT_FILE} 与契约一致（${findBlocks(current).length} 个生成块）`,
    );
    return 0;
  }
  writeFileSync(path, next);
  console.log(`已更新 ${CONTRACT_FILE}`);
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(main(process.argv.slice(2)));
}
