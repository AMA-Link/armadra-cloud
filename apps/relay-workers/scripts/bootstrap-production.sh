#!/usr/bin/env bash
# 一次性初始化生产中继 relay.armadra.com：生成全部 secret → 随首次部署一起上传 → 初始化 → 删掉首次专用 secret。
# 所有值只在本进程的变量里，部署时经 0700 私有目录里的临时文件交给 wrangler、随即删除，不进日志；生成后立刻把账号与口令打印一次给你保存。
# 已初始化过的部署要先 `pnpm exec wrangler delete --env=""` 清掉（会连同 Durable Object 数据一起删除）再重跑。
set -euo pipefail

cd "$(dirname "$0")/.."
export CLOUDFLARE_ACCOUNT_ID=a27d9dab9190150768845b62cfd57cfe
ORIGIN=https://relay.armadra.com
# 人机验证缺省关闭；要开启就先在 wrangler.jsonc 填站点密钥，并运行时设 ARMADRA_RELAY_TURNSTILE_SITE_KEY=<siteKey>。
SITE_KEY=${ARMADRA_RELAY_TURNSTILE_SITE_KEY:-}
ACCOUNT=${ARMADRA_RELAY_ACCOUNT:-owner}
W="pnpm exec wrangler"
WG="$(command -v wrangler || echo pnpm exec wrangler)"
# 本机代理的假 IP 解析会让新域名连不上，查询走 DoH。
CURL="curl -s -m 20 --doh-url https://cloudflare-dns.com/dns-query"

TS=""
if [ -n "$SITE_KEY" ]; then
  echo "== Turnstile secret（从你的 Cloudflare 账号读取并校验，不显示）"
  TS=$($WG turnstile widget get "$SITE_KEY" --json 2>/dev/null | python3 -c 'import sys,json; d=json.load(sys.stdin); d=d.get("result",d); print(d["secret"])')
  CHECK=$(curl -s -m 10 -X POST https://challenges.cloudflare.com/turnstile/v0/siteverify \
    --data-urlencode "secret=$TS" --data-urlencode "response=dummy" | python3 -c 'import sys,json; print(",".join(json.load(sys.stdin).get("error-codes",[])))')
  case "$CHECK" in
    *invalid-input-secret*) echo "Turnstile secret 无效（$CHECK），停止" >&2; exit 1 ;;
    *) echo "✓ Turnstile secret 校验通过（$CHECK）" ;;
  esac
else
  echo "== 人机验证：未开启（跳过 Turnstile）"
fi

echo "== 生成"
MASTER=$(openssl rand -hex 32)
PASS=$(openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c1-28)
cat <<EOF

>>> 登录信息（只显示这一次，现在就存进密码管理器）：
    地址：$ORIGIN
    账号：$ACCOUNT
    口令：$PASS

EOF
read -r -p "已保存？回车继续部署 …" _

echo "== 部署（secret 随首个版本上传）"
# wrangler 会起子进程读 secrets 文件，进程替换的 fd 传不过去；改用 0700 私有目录里的临时文件，部署完立刻删除。
SECDIR=$(mktemp -d); chmod 700 "$SECDIR"; trap 'rm -rf "$SECDIR"' EXIT
( umask 077; printf '%s\n%s\n%s\n%s\n' "$MASTER" "$ACCOUNT" "$PASS" "$TS" | python3 -c 'import sys,json; m,a,p,t=sys.stdin.read().split("\n")[:4]; d={"RELAY_MASTER_KEY":m,"RELAY_PERSONAL_ACCOUNT":a,"RELAY_PERSONAL_PASSWORD":p}; d.update({"RELAY_TURNSTILE_SECRET":t} if t else {}); print(json.dumps(d))' > "$SECDIR/secrets.json" )
$W deploy --env="" --secrets-file "$SECDIR/secrets.json" >/dev/null
rm -rf "$SECDIR"
unset MASTER PASS TS
echo "✓ 已部署 $ORIGIN"

echo "== 初始化"
R=""
for i in $(seq 1 30); do
  R=$($CURL "$ORIGIN/ready" || true)
  case "$R" in *'"ok":true'*) echo "✓ /ready $R"; break ;; esac
  sleep 4
done
case "$R" in *'"ok":true'*) ;; *) echo "初始化未完成：${R:-无响应}（secret 保留，可重跑本脚本）" >&2; exit 1 ;; esac
$CURL "$ORIGIN/.well-known/armadra-platform" | head -c 300; echo

echo "== 删除首次专用 secret"
$W secret delete RELAY_PERSONAL_PASSWORD --env="" <<<y >/dev/null 2>&1 && echo "· 已删除 RELAY_PERSONAL_PASSWORD"
$W secret delete RELAY_PERSONAL_ACCOUNT --env="" <<<y >/dev/null 2>&1 && echo "· 已删除 RELAY_PERSONAL_ACCOUNT"
echo
echo "完成：用上面保存的账号口令登录 $ORIGIN。"
