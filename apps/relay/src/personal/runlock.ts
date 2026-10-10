/**
 * `serve` 的运行标记：`<data-dir>/serve.pid`（0600，只有进程号）。`personal migrate` 看到活着的
 * `serve` 就拒绝——换存储要先停服。`serve` 正常退出时删掉；崩溃留下的标记按进程是否还活着判断。
 */
import { readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";

export const RUN_LOCK_FILE = "serve.pid";

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    // EPERM：进程在，只是不归我们管。
    return (error as NodeJS.ErrnoException).code === "EPERM";
  }
}

/** 写下本进程号；答一个释放函数（只删自己写的那份）。 */
export function holdRunLock(dataDir: string): () => void {
  const file = join(dataDir, RUN_LOCK_FILE);
  const mine = String(process.pid);
  writeFileSync(file, `${mine}\n`, { mode: 0o600 });
  return () => {
    try {
      if (readFileSync(file, "utf8").trim() === mine) rmSync(file);
    } catch {
      // 已经不在：不影响退出。
    }
  };
}

/** 正在运行的 `serve` 的进程号；没有、已退出或就是本进程答 null。 */
export function runningServe(dataDir: string): number | null {
  let text: string;
  try {
    text = readFileSync(join(dataDir, RUN_LOCK_FILE), "utf8");
  } catch {
    return null;
  }
  const pid = Number(text.trim());
  if (!Number.isInteger(pid) || pid <= 0 || pid === process.pid) return null;
  return alive(pid) ? pid : null;
}
