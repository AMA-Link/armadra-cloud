/**
 * 服务只经端口工作：内存参照跑一遍两套合同（端口语义、服务流程）。存储实现在各自的包里跑同样的
 * 合同（`@armadra/control-store-sqlite`）；Go 中继的存储实现由 `go/` 的合同测试与端到端覆盖。
 */
import { createMemoryPorts } from "./testing/memory-ports.ts";
import { portsContract } from "./testing/ports-contract.ts";
import { servicesContract } from "./testing/services-contract.ts";

portsContract("内存端口", createMemoryPorts);
servicesContract("内存端口", createMemoryPorts);
