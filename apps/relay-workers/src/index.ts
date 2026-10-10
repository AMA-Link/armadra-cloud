/**
 * Worker 入口：全部实现在 `@armadra/relay-runtime-workers`，这里只 re-export，`wrangler.jsonc`
 * 的 `durable_objects.bindings` 认这两个类名。
 */
export {
  ControlDO,
  default,
  SourceDO,
} from "@armadra/relay-runtime-workers/index";
