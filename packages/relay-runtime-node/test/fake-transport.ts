/** 单测用的隧道（假传输），流是 Node 的 {@link TunnelStream}。 */
import type {
  TunnelHooks,
  TunnelLimitsConfig,
} from "@armadra/relay-kernel/tunnel/tunnel";
import { fakeTunnel as kernelFakeTunnel } from "@armadra/relay-kernel/testing/fake-transport";

import { nodeStreams, type TunnelStream } from "../src/stream.ts";

export { OPEN } from "@armadra/relay-kernel/testing/fake-transport";

export function fakeTunnel(
  limits: Partial<TunnelLimitsConfig> = {},
  hooks: TunnelHooks = {},
) {
  return kernelFakeTunnel<TunnelStream>(limits, hooks, nodeStreams);
}
