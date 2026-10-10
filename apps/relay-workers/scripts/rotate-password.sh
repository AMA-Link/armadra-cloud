#!/usr/bin/env bash
# 修改 Workers 中继（缺省 relay.armadra.com）的账号口令（cloud-api §18），在你本机运行：
# 用当前账号登录拿一次会话 → 本机生成新口令 → 调改口令接口（账号的其它设备全部下线）→ 登出这次会话 →
# 只在你的终端打印一次新口令。
# 口令与令牌只在本进程的变量与 0700 私有目录的临时文件里，不进命令行参数、日志与 shell 历史。
# 中继要求人机验证时，按提示在浏览器里打开验证页，把页面给出的令牌粘回来。
#
# 用法: apps/relay-workers/scripts/rotate-password.sh
#   ARMADRA_RELAY_ORIGIN   中继地址（缺省 https://relay.armadra.com）
#   ARMADRA_RELAY_ACCOUNT  账号名（缺省 owner）
set -euo pipefail

ORIGIN=${ARMADRA_RELAY_ORIGIN:-https://relay.armadra.com}
ORIGIN=${ORIGIN%/}
ACCOUNT=${ARMADRA_RELAY_ACCOUNT:-owner}
DOH=${ARMADRA_RELAY_DOH:-https://cloudflare-dns.com/dns-query}
# 本机代理的假 IP 解析会让中继域名连不上，查询走 DoH。
CURL=(curl -sS -m 20 --doh-url "$DOH")

for tool in curl python3 openssl; do
  command -v "$tool" >/dev/null || {
    echo "缺少命令：$tool" >&2
    exit 1
  }
done

SECDIR=$(mktemp -d)
chmod 700 "$SECDIR"
trap 'rm -rf "$SECDIR"' EXIT

STATUS=000
# api METHOD PATH [访问令牌]：POST 的体从标准输入读；答复体在 $SECDIR/body，状态码记进
# $SECDIR/status（api 常在管道末端的子 shell 里跑），之后用 `took` 取回 $STATUS。
api() {
  local method=$1 path=$2 token=${3:-}
  local args=(-X "$method" -o "$SECDIR/body" -w '%{http_code}')
  if [ -n "$token" ]; then
    (
      umask 077
      printf 'authorization: Bearer %s\n' "$token" >"$SECDIR/auth"
    )
    args+=(-H "@$SECDIR/auth")
  fi
  if [ "$method" = POST ]; then
    args+=(-H 'content-type: application/json' --data-binary @-)
  fi
  : >"$SECDIR/body"
  local code
  code=$("${CURL[@]}" "${args[@]}" "$ORIGIN$path") || code=000
  rm -f "$SECDIR/auth"
  printf '%s' "$code" >"$SECDIR/status"
}
took() { STATUS=$(cat "$SECDIR/status"); }

# field 点分路径：从答复体取一个字段（没有就是空串）。
field() {
  python3 -c '
import json, sys
try:
    value = json.load(open(sys.argv[1]))
except Exception:
    value = None
for key in sys.argv[2].split("."):
    value = value.get(key) if isinstance(value, dict) else None
if isinstance(value, list):
    print(" ".join(str(item) for item in value))
elif value is not None:
    print(value)
' "$SECDIR/body" "$1"
}

# body 键…：值从标准输入按 NUL 分隔读进来，拼成 JSON；`challenge` 包成挑战令牌，空值不带。
body() {
  python3 -c '
import json, sys
values = sys.stdin.read().split("\0")
out = {}
for key, value in zip(sys.argv[1:], values):
    if value == "":
        continue
    if key == "challenge":
        out[key] = {"provider": "turnstile", "token": value}
    elif key == "device":
        out[key] = {"platform": "server", "name": value}
    else:
        out[key] = value
print(json.dumps(out))
' "$@"
}

fail_with() {
  local during=$1 code
  code=$(field code)
  case "$code" in
    credentials_invalid) echo "$during失败：账号或口令不对" >&2 ;;
    account_locked) echo "$during失败：尝试次数过多，账号暂时锁定，稍后再试" >&2 ;;
    rate_limited) echo "$during失败：请求太频繁，稍后再试" >&2 ;;
    challenge_required | challenge_invalid) echo "$during失败：人机验证没有通过" >&2 ;;
    "") echo "$during失败：中继没有答复（HTTP $STATUS）" >&2 ;;
    *) echo "$during失败：$code（HTTP $STATUS）" >&2 ;;
  esac
  exit 1
}

echo "== 中继：$ORIGIN"
api GET /.well-known/armadra-platform
took
[ "$STATUS" = 200 ] || fail_with "读取中继信息"
case " $(field capabilities) " in
  *" auth.password-change "*) ;;
  *)
    echo "这个中继还不支持改口令：先部署 0.3.10 及以上的中继" >&2
    exit 1
    ;;
esac
SCOPE=" $(field challenge.scope) "
SITE_KEY=$(field challenge.siteKey)

# challenge ACTION：中继要求时，请你在浏览器里过一次人机验证，把令牌粘回来（存进 $TOKEN）。
TOKEN=""
challenge() {
  local url="$ORIGIN/app/challenge?siteKey=$SITE_KEY&mode=copy&action=$1"
  printf '\n中继要求人机验证：在浏览器打开下面的地址，完成验证后复制页面上的令牌。\n  %s\n' "$url" >&2
  if [ -z "${ARMADRA_RELAY_NO_OPEN:-}" ] && command -v open >/dev/null; then
    open "$url" >/dev/null 2>&1 || true
  fi
  read -rsp "验证令牌：" TOKEN
  echo >&2
  [ -n "$TOKEN" ] || {
    echo "没有验证令牌，停止" >&2
    exit 1
  }
}

read -rsp "当前口令（$ACCOUNT）：" OLD
echo >&2
[ -n "$OLD" ] || {
  echo "没有输入口令，停止" >&2
  exit 1
}

echo "== 登录"
TOKEN=""
case "$SCOPE" in *" auth.login "*) challenge armadra-login ;; esac
printf '%s\0%s\0%s\0%s' "$ACCOUNT" "$OLD" "rotate-password" "$TOKEN" |
  body account password device challenge |
  api POST /v1/auth/login
took
[ "$STATUS" = 200 ] || fail_with "登录"
ACCESS=$(field session.accessToken)
[ -n "$ACCESS" ] || fail_with "登录"
echo "✓ 已登录"

echo "== 改口令"
NEW=""
for attempt in 1 2 3; do
  NEW=$(openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c1-28)
  TOKEN=""
  case "$SCOPE" in *" auth.changePassword "*) challenge armadra-password ;; esac
  printf '%s\0%s\0%s' "$OLD" "$NEW" "$TOKEN" |
    body password newPassword challenge |
    api POST /v1/auth/password "$ACCESS"
  took
  [ "$STATUS" = 200 ] && break
  # 随机口令碰巧含账号名或在常见表里：换一个再试。
  case "$(field code)" in
    password_contains_name | password_too_common) [ "$attempt" -lt 3 ] && continue ;;
  esac
  fail_with "改口令"
done
[ "$STATUS" = 200 ] || fail_with "改口令"
ACCESS=$(field session.accessToken)
unset OLD TOKEN
echo "✓ 口令已更新；账号在其它设备上的登录已全部失效"

# 这次会话只为改口令而建，用完登出（失败不影响结果）。
if [ -n "$ACCESS" ]; then
  printf '{}' | api POST /v1/auth/logout "$ACCESS" || true
fi
unset ACCESS

# 新口令只写到终端（/dev/tty）；没有终端时写到标准输出。
if { exec 3>/dev/tty; } 2>/dev/null; then :; else exec 3>&1; fi
cat >&3 <<EOF

>>> 新口令（只显示这一次，现在就存进密码管理器）：
    地址：$ORIGIN
    账号：$ACCOUNT
    口令：$NEW

EOF
exec 3>&-
unset NEW
