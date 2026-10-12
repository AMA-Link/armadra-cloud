/**
 * 按小写名取值的请求头（Node 的 `IncomingHttpHeaders` 与之结构相同；Workers 由
 * `Headers` 构造）。同名多行时 Node 已用 `, ` 合并，只有 `set-cookie` 是数组。
 *
 * 边缘只读其中几样（`host`、`origin`、`sec-websocket-protocol`、预检的请求方法）；
 * 0.5.0 起不再把请求头转给源（中继只转端到端通道的密文，cloud-api §20）。
 */
export type HeaderBag = Readonly<Record<string, string | string[] | undefined>>;
