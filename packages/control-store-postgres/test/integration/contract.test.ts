/** PostgreSQL 端口跑控制核的两套合同（端口语义、服务流程），与内存参照、SQLite 同一批用例。 */
import { portsContract } from "@armadra/control-core/testing/ports-contract";
import { servicesContract } from "@armadra/control-core/testing/services-contract";
import { afterAll, afterEach } from "vitest";

import { contractDatabase } from "./harness.ts";

const database = contractDatabase();
afterEach(() => database.reset());
afterAll(() => database.reset());

portsContract("PostgreSQL 端口", () => database.ports());
servicesContract("PostgreSQL 端口", () => database.ports());
