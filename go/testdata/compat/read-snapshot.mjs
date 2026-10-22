// 用 TypeScript 的存储读一个库，把整个租户的快照以 JSON 打到标准输出（Go 的兼容测试拿它比对
// 「Go 写的库 TypeScript 读得出来」）：
//
//   node go/testdata/compat/read-snapshot.mjs sqlite <库文件> [租户]
//   node go/testdata/compat/read-snapshot.mjs postgres <连接串> <schema> [租户]
//
// 另外打出签名钥是否可用（Signer 能加载私钥）、迁移账本条数，作为 TypeScript 真的「打开」了库的证据。
// 连接串里有口令：只经命令行参数传入，不打印。
import { createPrivateKey } from "node:crypto";

import { openDatabase } from "../../../packages/control-store-sqlite/src/open.ts";
import { exportTenant as exportSqlite } from "../../../packages/control-store-sqlite/src/tenants.ts";
import { openPostgres } from "../../../packages/control-store-postgres/src/open.ts";
import { exportTenant as exportPostgres } from "../../../packages/control-store-postgres/src/tenants.ts";

const [kind, ...rest] = process.argv.slice(2);

function report(snapshot, migrations) {
  const keysUsable = snapshot.signingKeys.every((key) => {
    createPrivateKey({
      key: Buffer.from(key.privateKeyPkcs8, "base64"),
      format: "der",
      type: "pkcs8",
    });
    return true;
  });
  process.stdout.write(
    JSON.stringify({ migrations, keysUsable, snapshot }, null, 0),
  );
}

if (kind === "sqlite") {
  const [path, tenant = "default"] = rest;
  const db = openDatabase(path);
  try {
    const applied = db.get("SELECT count(*) AS n FROM relay_migrations");
    report(exportSqlite(db, tenant), Number(applied?.n));
  } finally {
    db.close();
  }
} else if (kind === "postgres") {
  const [url, schema, tenant = "default"] = rest;
  const db = await openPostgres({ url, schema, max: 1 });
  try {
    const [applied] =
      await db.sql`SELECT count(*)::int AS n FROM cloud_migrations`;
    report(await exportPostgres(db.sql, tenant), Number(applied?.n));
  } finally {
    await db.close();
  }
} else {
  console.error(
    "用法: read-snapshot.mjs sqlite <库文件> [租户] | postgres <连接串> <schema> [租户]",
  );
  process.exit(64);
}
