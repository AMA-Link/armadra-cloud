import type { RelayEnv } from "@armadra/relay-runtime-workers/env";

declare global {
  namespace Cloudflare {
    interface Env extends RelayEnv {}
  }
}
