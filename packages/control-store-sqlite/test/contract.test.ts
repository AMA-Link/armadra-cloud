/**
 * SQLite 端口跑控制核的两套合同（端口语义、服务流程），与内存参照同一批用例。
 */
import { portsContract } from "@armadra/control-core/testing/ports-contract";
import { servicesContract } from "@armadra/control-core/testing/services-contract";
import { afterAll } from "vitest";

import { tempDatabases } from "./harness.ts";

const databases = tempDatabases();
afterAll(() => databases.cleanup());

portsContract("SQLite 端口", () => databases.ports());
servicesContract("SQLite 端口", () => databases.ports());
