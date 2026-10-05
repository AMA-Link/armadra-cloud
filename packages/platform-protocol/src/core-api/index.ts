// core 契约 §31–§33 的请求 / 响应 schema。cloud / relay、页面、手机与 Armadra 的
// `packages/shared/src/contract/{cloud,sources}.ts` 都从这里 import 同一份对象。
export * from "./cloud-login.ts";
export * from "./cloud-register.ts";
export * from "./cloud-manage.ts";
export * from "./sources.ts";
