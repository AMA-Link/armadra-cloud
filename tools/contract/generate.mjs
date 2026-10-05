/**
 * 从协议包的 `/v1` 契约生成两样东西：
 *
 * - docs/contracts/cloud-openapi.json：`@orpc/openapi` 生成的 OpenAPI 文档（错误体
 *   按本仓的线上形状 `{ code, message, requestId?, details? }` 描述）；
 * - docs/contracts/cloud-api.md 的生成块 `<!-- rpc:begin contract=§N -->` …
 *   `<!-- rpc:end -->`：每节一张「procedure / 方法与路径 / scope / 错误码」表。
 *
 *   node tools/contract/generate.mjs           重写
 *   node tools/contract/generate.mjs --check   与契约不一致就失败
 *
 * 读的是协议包的构建产物（`pnpm libs:build` 之后）。`@orpc/*` 只能出现在
 * tools/contract/ 这类门面里（AGENTS.md）。
 */
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("../../", import.meta.url));
export const CONTRACT_FILE = "docs/contracts/cloud-api.md";
export const OPENAPI_FILE = "docs/contracts/cloud-openapi.json";

const BLOCK = /<!-- rpc:begin contract=(§\d+) -->\n([\s\S]*?)<!-- rpc:end -->/g;

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

const cell = (value) => String(value).replaceAll("|", "\\|");

/** 一节的形状表；没有 procedure 的节写一行说明。 */
export function renderSection(section, procedures, sectionOf) {
  const rows = procedures.filter((entry) => sectionOf(entry.key) === section);
  if (rows.length === 0) return "_本节没有 `/v1` procedure。_\n";
  const lines = [
    "| procedure | 方法与路径 | scope | 错误码 |",
    "| --- | --- | --- | --- |",
    ...rows.map(
      (entry) =>
        `| \`${entry.key}\` | \`${entry.method} ${cell(entry.path)}\` | ${entry.meta.scope.map((s) => `\`${s}\``).join(" / ")} | ${
          entry.errorCodes.length === 0
            ? "—"
            : entry.errorCodes.map((code) => `\`${code}\``).join("、")
        } |`,
    ),
    "",
    "输入与输出的完整 schema 见 [cloud-openapi.json](cloud-openapi.json)。",
  ];
  return `${lines.join("\n")}\n`;
}

/** 本仓的错误体：`{ code, message, requestId?, details? }`，`code` 取该条声明的码。 */
function errorBodySchema(definedErrors) {
  return {
    type: "object",
    properties: {
      code:
        definedErrors.length > 0
          ? { type: "string", enum: definedErrors.map(([code]) => code) }
          : { type: "string" },
      message: { type: "string" },
      requestId: { type: "string" },
      details: {},
    },
    required: ["code", "message"],
  };
}

export async function generateOpenApi(contract, version) {
  const { OpenAPIGenerator } = await import("@orpc/openapi");
  const { ZodToJsonSchemaConverter } = await import("@orpc/zod/zod4");
  const generator = new OpenAPIGenerator({
    schemaConverters: [new ZodToJsonSchemaConverter()],
  });
  const document = await generator.generate(contract, {
    info: {
      title: "armadra-cloud /v1",
      version,
      description:
        "由 tools/contract/generate.mjs 从 @armadra/platform-protocol 的 cloudContract 生成，不要手改。",
    },
    customErrorResponseBodySchema: (definedErrors) =>
      errorBodySchema(definedErrors),
  });
  return `${JSON.stringify(document, null, 2)}\n`;
}

async function main(argv) {
  const check = argv.includes("--check");
  const api = await import("@armadra/platform-protocol/cloud-api");
  const manifest = JSON.parse(
    readFileSync(join(root, "packages/platform-protocol/package.json"), "utf8"),
  );
  const procedures = api.listProcedures(api.cloudContract);
  const contractPath = join(root, CONTRACT_FILE);
  const openapiPath = join(root, OPENAPI_FILE);
  if (!existsSync(contractPath)) {
    console.error(`契约文件缺失：${CONTRACT_FILE}`);
    return 1;
  }
  const currentMarkdown = readFileSync(contractPath, "utf8");
  // 表格按 Prettier 的对齐写回，`pnpm format:check` 与 `--check` 才不会互相打架。
  const { format } = await import("prettier");
  const nextMarkdown = await format(
    fill(currentMarkdown, (section) =>
      renderSection(section, procedures, api.sectionOf),
    ),
    { parser: "markdown", filepath: contractPath },
  );
  const covered = new Set(findBlocks(currentMarkdown).map((b) => b.section));
  const orphans = procedures.filter(
    (entry) => !covered.has(api.sectionOf(entry.key)),
  );
  if (orphans.length > 0) {
    console.error(
      `这些 procedure 的节在 ${CONTRACT_FILE} 里没有生成块：${orphans.map((e) => e.key).join("、")}`,
    );
    return 1;
  }
  const nextOpenApi = await generateOpenApi(
    api.cloudContract,
    manifest.version,
  );
  const currentOpenApi = existsSync(openapiPath)
    ? readFileSync(openapiPath, "utf8")
    : "";
  if (check) {
    const stale = [];
    if (nextMarkdown !== currentMarkdown) stale.push(CONTRACT_FILE);
    if (nextOpenApi !== currentOpenApi) stale.push(OPENAPI_FILE);
    if (stale.length > 0) {
      console.error(
        `${stale.join("、")} 与契约不一致；运行 pnpm contract:generate。`,
      );
      return 1;
    }
    console.log(
      `契约文档一致（${procedures.length} 条 procedure，${covered.size} 个生成块）`,
    );
    return 0;
  }
  writeFileSync(contractPath, nextMarkdown);
  writeFileSync(openapiPath, nextOpenApi);
  console.log(`已更新 ${CONTRACT_FILE} 与 ${OPENAPI_FILE}`);
  return 0;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.exit(await main(process.argv.slice(2)));
}
