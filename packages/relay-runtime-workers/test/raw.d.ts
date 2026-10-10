/** 测试里用到的 Vite 能力（Workers 插件经 Vite 转换测试文件）。 */
declare module "*?raw" {
  const text: string;
  export default text;
}

interface ImportMeta {
  glob(
    pattern: string,
    options: { query: "?raw"; import: "default"; eager: true },
  ): Record<string, unknown>;
}
