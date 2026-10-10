import type { RelayEnv } from "../src/env.ts";

declare global {
  namespace Cloudflare {
    interface Env extends RelayEnv {}
  }
}
