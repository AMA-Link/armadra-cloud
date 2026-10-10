/**
 * Cloudflare Workers 中继的入口：默认导出给 Worker，两个 Durable Object 类给 `wrangler.jsonc` 的
 * `durable_objects.bindings`。`apps/relay-workers` 只 re-export 这里。
 */
import type { RelayEnv } from "./env.ts";
import { handleRequest } from "./router.ts";

export { ControlDO } from "./control-do.ts";
export { SourceDO } from "./source-do.ts";
export type { RelayEnv } from "./env.ts";

export default {
  fetch(request: Request, env: RelayEnv): Promise<Response> {
    return handleRequest(request, env);
  },
} satisfies ExportedHandler<RelayEnv>;
