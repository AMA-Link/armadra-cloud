// 生成兼容夹具（TypeScript 版写出来的库，给 Go 的兼容测试读写）：
//
//   node go/testdata/compat/make-fixtures.mjs [--postgres-url <连接串>]
//
// 写出（都在本目录）：
//   state.json      个人中转的状态文件（schema v1），Go 的 statefile 只读导入它；
//   relay.db        TypeScript 的 SQLite 存储建出来的库（VACUUM INTO，单文件）；
//   postgres.sql    TypeScript 的 PostgreSQL 迁移器建出来的 schema 的转储（建表 + 账本 + 数据），
//                   Go 测试把它还原进一个随机名字的 schema，再用 Go 打开；
//   expected.json   测试值（口令、令牌明文、固定 id），Go 的兼容测试按它登录、刷新。
//
// 连接串缺省取 ARMADRA_CLOUD_TEST_DATABASE_URL；没有就只写前三项以外的（不更新 postgres.sql）。
// 需要先 pnpm install 与 pnpm libs:build。夹具里只有测试值，不含真实口令或密钥。
// 重新生成只在合同的数据形状变了（迁移或记录字段）时做，并同 PR 提交。
import { randomBytes } from "node:crypto";
import { readFileSync, rmSync, writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { stateToSnapshot } from "./snapshot.ts";
import { stateSchema } from "./state-schema.ts";
import { openDatabase } from "../../../packages/control-store-sqlite/src/open.ts";
import { createSqlitePorts } from "../../../packages/control-store-sqlite/src/ports.ts";
import { importTenant as importSqlite } from "../../../packages/control-store-sqlite/src/tenants.ts";
import {
  connectPostgres,
  openPostgres,
} from "../../../packages/control-store-postgres/src/open.ts";
import { createPostgresPorts } from "../../../packages/control-store-postgres/src/ports.ts";
import { importTenant as importPostgres } from "../../../packages/control-store-postgres/src/tenants.ts";
import { LEDGER_DDL } from "../../../packages/control-store-postgres/src/migrate.ts";

import {
  buildState,
  IDS,
  NOW,
  PASSWORD,
  SECRETS,
  TRAFFIC,
} from "./dataset.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const TENANT = "default";
const argv = process.argv.slice(2);
const urlAt = argv.indexOf("--postgres-url");
const postgresUrl =
  (urlAt === -1 ? undefined : argv[urlAt + 1]) ??
  process.env.ARMADRA_CLOUD_TEST_DATABASE_URL;

const state = stateSchema.parse(await buildState());
const snapshot = stateToSnapshot(state);

writeFileSync(join(here, "state.json"), `${JSON.stringify(state, null, 2)}\n`);
writeFileSync(
  join(here, "expected.json"),
  `${JSON.stringify(
    {
      note: "兼容夹具的测试值，不含真实口令或密钥",
      nowMs: NOW,
      tenant: TENANT,
      password: PASSWORD,
      ids: IDS,
      secrets: SECRETS,
      revision: state.revision,
    },
    null,
    2,
  )}\n`,
);

// ---------------------------------------------------------------- SQLite
{
  const dir = mkdtempSync(join(tmpdir(), "compat-sqlite-"));
  try {
    const db = openDatabase(join(dir, "build.db"), {
      create: true,
      now: () => NOW,
    });
    importSqlite(db, TENANT, snapshot);
    await createSqlitePorts(db).traffic.upsert(TENANT, TRAFFIC);
    const target = join(dir, "relay.db");
    db.backup(target);
    db.close();
    rmSync(join(here, "relay.db"), { force: true });
    writeFileSync(join(here, "relay.db"), readFileSync(target), {
      mode: 0o600,
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

// ------------------------------------------------------------ PostgreSQL
const literal = (value, type) => {
  if (value === null || value === undefined) return "NULL";
  if (type === "boolean") return value ? "true" : "false";
  if (type === "integer" || type === "bigint") return String(value);
  return `'${String(value).replaceAll("'", "''")}'`;
};

if (postgresUrl) {
  const schema = `compat_${randomBytes(6).toString("hex")}`;
  const admin = connectPostgres({ url: postgresUrl, max: 1 });
  await admin.unsafe(`CREATE SCHEMA ${schema}`);
  try {
    const db = await openPostgres({
      url: postgresUrl,
      schema,
      now: () => NOW,
    });
    await importPostgres(db.sql, TENANT, snapshot);
    await createPostgresPorts(db.sql).traffic.upsert(TENANT, TRAFFIC);
    const out = [
      "-- 由 go/testdata/compat/make-fixtures.mjs 生成：TypeScript 的 PostgreSQL 迁移器建出来的 schema 的转储。",
      "-- 还原进一个空 schema 即可（建表是迁移文件原文，账本行是 TypeScript 迁移器写的）。",
      "",
      `${LEDGER_DDL};`,
      "",
    ];
    const migrationsDir = join(
      here,
      "../../../packages/control-store-postgres/migrations",
    );
    const ledger = await db.sql.unsafe(
      "SELECT id, name, sha384, applied_at_ms FROM cloud_migrations ORDER BY id",
    );
    for (const row of ledger) {
      out.push(readFileSync(join(migrationsDir, `${row.name}.sql`), "utf8"));
    }
    for (const row of ledger) {
      out.push(
        `INSERT INTO cloud_migrations (id, name, sha384, applied_at_ms) VALUES (${row.id}, ${literal(row.name)}, ${literal(row.sha384)}, ${row.applied_at_ms});`,
      );
    }
    out.push("");
    for (const [table, order] of [
      ["tenants", "seq"],
      ["accounts", "seq"],
      ["signing_keys", "seq"],
      ["sources", "seq"],
      ["registration_tokens", "seq"],
      ["devices", "seq"],
      ["links", "seq"],
      [
        "relay_traffic_buckets",
        "bucket_start_ms, node, source_id, subject, device_id, kind",
      ],
    ]) {
      const columns = await db.sql.unsafe(
        `SELECT column_name, data_type FROM information_schema.columns
         WHERE table_schema = current_schema() AND table_name = '${table}' AND column_name <> 'seq'
         ORDER BY ordinal_position`,
      );
      const names = columns.map((column) => column.column_name);
      const rows = await db.sql.unsafe(
        `SELECT ${names.join(", ")} FROM ${table} ORDER BY ${order}`,
      );
      for (const row of rows) {
        const values = columns.map((column) =>
          literal(row[column.column_name], column.data_type),
        );
        out.push(
          `INSERT INTO ${table} (${names.join(", ")}) VALUES (${values.join(", ")});`,
        );
      }
    }
    writeFileSync(join(here, "postgres.sql"), `${out.join("\n")}\n`);
    await db.close();
  } finally {
    await admin.unsafe(`DROP SCHEMA ${schema} CASCADE`);
    await admin.end({ timeout: 5 });
  }
} else {
  console.error("没有 PostgreSQL 连接串：postgres.sql 保持原样");
}
console.log("兼容夹具已写入", here);
