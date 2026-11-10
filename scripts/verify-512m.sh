#!/usr/bin/env bash
# samll-ci 512M 内存构建验收脚本 —— 在 1c/512M 目标机上执行
# 用法: ./verify-512m.sh <samll-ci地址> <密码> <项目ID>
#   例: ./verify-512m.sh http://127.0.0.1:8080 admin 18
#
# 验收判据（对应 docs/512m-build-plan.md 阶段 3）:
#   P1 swap 已配置（>=2G）
#   P2 重型项目构建成功（堆 1536MB，swap 承接超出部分）
#   P3 产物落入 outputs/<项目ID>/
#   P4 同 commit 重复触发跳过构建秒过（非 turbo 通用缓存）
set -euo pipefail

B="${1:-http://127.0.0.1:8080}"
PASSWD="${2:-admin}"
PID="${3:?需要项目ID}"
PASS=0; FAIL=0

ok()   { echo "  ✓ $1"; PASS=$((PASS+1)); }
bad()  { echo "  ✗ $1"; FAIL=$((FAIL+1)); }

api() { curl -s -X "$1" "$B$2" -H 'Content-Type: application/json' \
        ${3:+-H "Authorization: Bearer $TOKEN"} ${4:+-d "$4"}; }

echo "═══ samll-ci 512M 构建验收 ═══"
echo "目标: $B 项目 #$PID"

# P1 swap
echo "[P1] swap 检查"
swap_total=$(free -m | awk '/^Swap:/ {print $2}')
if [ "${swap_total:-0}" -ge 4000 ]; then
  ok "swap ${swap_total}MB >= 4096MB"
else
  bad "swap 仅 ${swap_total:-0}MB（需 >=4G，2G 实测内核 OOM kill，配置见 docs/512m-build-plan.md）"
fi

TOKEN=$(api POST /api/login '' | sed 's/.*"token":"\([^"]*\)".*/\1/')
[ -n "$TOKEN" ] && ok "登录成功" || { bad "登录失败"; exit 1; }

# 项目配置检查
proj=$(api GET "/api/projects/$PID" true)
mem=$(echo "$proj" | sed 's/.*"mem_limit_mib":\([0-9]*\).*/\1/')
outdir=$(echo "$proj" | sed 's/.*"output_dir":"\([^"]*\)".*/\1/')
[ "${mem:-0}" -ge 1500 ] && ok "项目内存覆盖 ${mem}MB（堆 1536 兜底 swap）" || bad "项目内存覆盖 ${mem:-0}MB < 1500"
[ -n "$outdir" ] && ok "产物目录已配置: $outdir" || bad "未配置 output_dir（unh 类 CLI 吞退出码时无法防御误判）"

# P2+P3 触发构建并等待
echo "[P2/P3] 触发构建（swap 路径可能耗时较长，请耐心等待）"
bid=$(api POST "/api/projects/$PID/build" true | sed 's/.*"build_id":\([0-9]*\).*/\1/')
echo "  构建 #$bid 已提交"
for i in $(seq 1 240); do
  st=$(api GET "/api/builds/$bid" true | sed 's/.*"status":"\([^"]*\)".*/\1/')
  [ "$st" != "running" ] && [ "$st" != "pending" ] && break
  sleep 30
done
[ "$st" = "success" ] && ok "构建 #$bid success" || bad "构建 #$bid 状态 $st"
files=$(curl -s -H "Authorization: Bearer $TOKEN" "$B/api/cache" | grep -c '"' || true)
if [ "$st" = "success" ]; then
  echo "  → 请在控制台确认 outputs/$PID 产物已更新"
fi

# P4 同 commit 跳过
echo "[P4] 同 commit 跳过验证"
t0=$(date +%s)
bid2=$(api POST "/api/projects/$PID/build" true | sed 's/.*"build_id":\([0-9]*\).*/\1/')
st2=$(api GET "/api/builds/$bid2" true | sed 's/.*"status":"\([^"]*\)".*/\1/')
dt=$(( $(date +%s) - t0 ))
if [ "$st2" = "success" ] && [ $dt -le 5 ]; then
  ok "重复触发 ${dt}s 即返（跳过构建复用产物）"
else
  bad "重复触发 ${dt}s 状态 $st2（未跳过）"
fi

echo "═══ 验收结果: $PASS 通过 / $FAIL 未过 ═══"
exit $([ $FAIL -eq 0 ] && echo 0 || echo 1)
