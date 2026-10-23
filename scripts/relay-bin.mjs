// 中继二进制的解析与构建（pnpm relay:personal 与 pnpm dev 共用）。
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { join, resolve } from "node:path";

/** 中继二进制：ARMADRA_RELAY_BIN，否则从 go/ 构建到 go/bin/armadra-relay。 */
export function resolveRelayBin(root) {
  const given = process.env.ARMADRA_RELAY_BIN?.trim();
  if (given) {
    const bin = resolve(root, given);
    if (!existsSync(bin)) {
      console.error(`ARMADRA_RELAY_BIN 指向的文件不存在：${bin}`);
      process.exit(1);
    }
    return bin;
  }
  const bin = join(root, "go", "bin", "armadra-relay");
  // 与镜像一样，/health 与 version 报根 package.json 的版本。
  const { version } = JSON.parse(
    readFileSync(join(root, "package.json"), "utf8"),
  );
  const built = spawnSync(
    "go",
    [
      "build",
      "-ldflags",
      `-X main.version=${version}`,
      "-o",
      bin,
      "./cmd/armadra-relay",
    ],
    { cwd: join(root, "go"), stdio: "inherit" },
  );
  if (built.error || built.status !== 0) {
    console.error(
      `构建中继失败（要 Go 工具链，见 mise.toml）：${built.error?.message ?? `退出码 ${built.status}`}`,
    );
    process.exit(1);
  }
  return bin;
}
