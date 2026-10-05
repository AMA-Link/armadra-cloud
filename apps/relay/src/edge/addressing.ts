/**
 * 客户端地址 → sourceId（中继设计 §4.1，平台设计 D23）。两种写法，边缘只认 {@link Addressing}：
 *
 * - saas：`Host` 是 `<sourceId>.src.<RELAY_DOMAIN>`，路径原样转发；
 * - personal：路径前缀 `/s/<sourceId>`，剥掉前缀后再转发（`/s/<id>` 与 `/s/<id>?q` 都转成 `/…`）。
 *
 * 都不匹配 → null（HTTP 404 / 拒绝升级）。sourceId 只认 32 位小写 hex。
 */
export interface AddressTarget {
  sourceId: string;
  /** 转给源的路径（含 query）。 */
  path: string;
}

export interface Addressing {
  resolve(request: {
    url: string;
    host: string | undefined;
  }): AddressTarget | null;
}

const PATH_PREFIX = /^\/s\/([0-9a-f]{32})(?=$|[/?])/;

export function pathAddressing(): Addressing {
  return {
    resolve({ url }) {
      const match = PATH_PREFIX.exec(url);
      if (!match?.[1]) return null;
      const rest = url.slice(match[0].length);
      return {
        sourceId: match[1],
        path: rest.startsWith("/") ? rest : `/${rest}`,
      };
    },
  };
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

export function hostAddressing(domain: string): Addressing {
  const pattern = new RegExp(
    `^([0-9a-f]{32})\\.src\\.${escapeRegExp(domain.toLowerCase())}$`,
  );
  return {
    resolve({ url, host }) {
      if (!host || !url.startsWith("/")) return null;
      const name = host.toLowerCase().replace(/:\d+$/, "");
      const match = pattern.exec(name);
      return match?.[1] ? { sourceId: match[1], path: url } : null;
    },
  };
}
