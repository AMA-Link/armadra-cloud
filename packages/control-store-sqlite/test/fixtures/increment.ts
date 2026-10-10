// 多进程并发写的子进程：打开同一个库，把链接的 uses 加 N 次（每次一个读-改-写事务）。
import { DEFAULT_TENANT } from "@armadra/control-core/ports";

import { openDatabase } from "../../src/open.ts";
import { createSqlitePorts } from "../../src/ports.ts";

const [path, linkId, times] = process.argv.slice(2);
const db = openDatabase(path ?? "");
const ports = createSqlitePorts(db);
for (let index = 0; index < Number(times); index += 1)
  await ports.links.update(DEFAULT_TENANT, linkId ?? "", (draft) => {
    if (draft) draft.uses += 1;
  });
db.close();
