/**
 * 就绪检查：每个依赖一个探测函数，带超时并行跑。结果只报「ok / fail」与耗时，
 * 不回显错误原文（连接串可能出现在驱动的错误消息里）。
 */
export type Probe = () => Promise<unknown>;

export interface ReadinessResult {
  ok: boolean;
  checks: Record<string, { ok: boolean; ms: number }>;
}

export async function runProbes(
  probes: Record<string, Probe>,
  timeoutMs = 2_000,
): Promise<ReadinessResult> {
  const entries = await Promise.all(
    Object.entries(probes).map(async ([name, probe]) => {
      const started = performance.now();
      let timer: NodeJS.Timeout | undefined;
      const timeout = new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error("timeout")), timeoutMs);
      });
      try {
        await Promise.race([probe(), timeout]);
        return [
          name,
          { ok: true, ms: Math.round(performance.now() - started) },
        ] as const;
      } catch {
        return [
          name,
          { ok: false, ms: Math.round(performance.now() - started) },
        ] as const;
      } finally {
        clearTimeout(timer);
      }
    }),
  );
  const checks = Object.fromEntries(entries);
  return { ok: entries.every(([, check]) => check.ok), checks };
}
