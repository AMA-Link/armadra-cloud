package cli

// Usage 是顶层用法（设计 §2.1）。
const Usage = `用法: armadra-cloud <命令> [选项]

  serve            起控制面（HTTP、作业、Redis 订阅）
  migrate          执行数据库迁移（只加不改）
  keys rotate      轮换签名钥（新钥立即生效，旧钥 1 小时内仍在 JWKS 里）
  nodes token      给中继节点签节点令牌（--node <id> --days <n>，缺省 30）
  nodes revoke     作废一枚节点令牌（--jti <jti>）
  operators add    给已有账号加运营范围（--email <地址>）
  operators totp   为运营账号登记两步验证（在终端打印一次 otpauth 地址）
  health           探活（容器健康检查用）
  version          打印版本

配置全部来自环境变量（前缀 ARMADRA_CLOUD_），口令、令牌与主密钥用 *_FILE 或环境变量给。
退出码: 0 成功，64 用法或配置错误，1 运行出错。`
