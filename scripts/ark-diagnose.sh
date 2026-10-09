#!/usr/bin/env bash
# 诊断本机到火山方舟（Ark）接口的连通性，区分"代理规则问题"与"网络/对端问题"。
#
#   scripts/ark-diagnose.sh                  # 读取 ~/.config/yanshi/ark.env
#   ARK_ENV=/path/to/ark.env scripts/ark-diagnose.sh
#   MODEL=doubao-seed-2.1-lite scripts/ark-diagnose.sh   # 连通后用该模型发一次最小对话
#
# 步骤：1 系统路径（经代理）→ 2 DNS（系统 / 公共）→ 3 代理与路由 → 4 绕过代理直连真实 IP
#       → 5 对照组（火山其他域名）→ 6 结论。输出中不会出现 API key。
set -uo pipefail

ARK_ENV=${ARK_ENV:-${HOME}/.config/yanshi/ark.env}
[ -f "${ARK_ENV}" ] && . "${ARK_ENV}"
: "${ARK_BASE_URL:=https://ark.cn-beijing.volces.com/api/coding/v3}"
MODEL=${MODEL:-doubao-seed-2.1-lite}
if [ -z "${ARK_API_KEY:-}" ]; then
  echo "未找到 ARK_API_KEY（${ARK_ENV} 或环境变量）" >&2
  exit 2
fi
HOST=$(echo "${ARK_BASE_URL}" | sed -E 's#^https?://([^/:]+).*#\1#')
CONTROL_HOST=open.volcengineapi.com
TIMEOUT=${TIMEOUT:-12}

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok() { printf '  \033[32m✓\033[0m %s\n' "$*"; }
bad() { printf '  \033[31m✗\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }

# probe <描述> <curl 参数...>：请求 /models，打印状态码、耗时与错误；返回 0 表示拿到了 HTTP 响应。
probe() {
  local desc=$1; shift
  local out code err
  out=$(curl -sS -o /dev/null -m "${TIMEOUT}" -w '%{http_code} %{remote_ip} %{time_total}' \
    -H "Authorization: Bearer ${ARK_API_KEY}" "$@" 2>/tmp/ark-diagnose.err)
  code=${out%% *}
  err=$(sed "s/${ARK_API_KEY}/***/g" /tmp/ark-diagnose.err | tail -1)
  if [ "${code}" != "000" ] && [ -n "${code}" ]; then
    ok "${desc} → HTTP ${code}（${out#* }）"
    return 0
  fi
  bad "${desc} → 无响应：${err:-超时}"
  return 1
}

public_ips() { # 用公共 DNS 解析真实 IP（绕过代理软件的 fake-ip DNS）
  local h=$1
  for ns in 223.5.5.5 223.6.6.6 119.29.29.29 114.114.114.114; do
    dig +short +time=2 +tries=1 @"${ns}" "${h}" A 2>/dev/null
  done | grep -E '^[0-9]+(\.[0-9]+){3}$' | sort -u
}

# 物理网卡：默认路由中非 utun/tun 的那一条（TUN 模式代理会占用默认路由）。
physical_if() {
  if command -v netstat >/dev/null; then
    netstat -rn -f inet 2>/dev/null | awk '$1=="default" && $NF !~ /^(utun|tun|ppp|ipsec)/ {print $NF; exit}'
  fi
  if command -v ip >/dev/null; then
    ip route show default 2>/dev/null | awk '{for (i=1;i<NF;i++) if ($i=="dev" && $(i+1) !~ /^tun/) {print $(i+1); exit}}'
  fi
}

echo "目标：${ARK_BASE_URL}  （host ${HOST}）"

bold "1. 系统路径（与 yanshi 相同：经过系统代理 / TUN）"
SYSTEM_OK=0
probe "GET /models" "${ARK_BASE_URL}/models" && SYSTEM_OK=1

bold "2. DNS"
SYS_IP=$( (dscacheutil -q host -a name "${HOST}" 2>/dev/null | awk '/ip_address/ {print $2; exit}') || true)
[ -z "${SYS_IP}" ] && SYS_IP=$(getent hosts "${HOST}" 2>/dev/null | awk '{print $1; exit}')
info "系统解析：${SYS_IP:-无}"
case "${SYS_IP}" in
  198.18.* | 198.19.*) info "→ 这是代理软件的 fake-ip（198.18.0.0/15），流量由代理接管" ;;
esac
REAL_IPS=$(public_ips "${HOST}")
info "公共 DNS 解析：$(echo ${REAL_IPS})"

bold "3. 代理与路由"
if command -v scutil >/dev/null; then
  scutil --proxy 2>/dev/null | awk '/HTTPSEnable|HTTPSProxy|HTTPSPort|ProxyAutoConfigEnable/ {printf "    %s %s %s\n", $1, $2, $3}'
fi
env | grep -iE '^(https?|all)_proxy=' | sed 's/^/    /'
DEF_IF=$(route -n get default 2>/dev/null | awk '/interface:/ {print $2}')
PHYS_IF=$(physical_if)
info "默认路由网卡：${DEF_IF:-未知}；物理网卡：${PHYS_IF:-未找到}"
case "${DEF_IF}" in utun* | tun*) info "→ 默认路由在虚拟网卡上，代理工作在 TUN 模式" ;; esac

bold "4. 绕过代理，直连真实 IP（绑定物理网卡，指定公共 DNS 解析出的 IP）"
DIRECT_OK=0
DIRECT_ARGS=(--noproxy '*')
[ -n "${PHYS_IF}" ] && DIRECT_ARGS+=(--interface "${PHYS_IF}")
if [ -z "${REAL_IPS}" ]; then
  bad "公共 DNS 无法解析 ${HOST}，跳过"
else
  for ip in ${REAL_IPS}; do
    probe "${ip}:443 HTTPS" "${DIRECT_ARGS[@]}" --resolve "${HOST}:443:${ip}" "${ARK_BASE_URL}/models" && DIRECT_OK=1
    if [ "${DIRECT_OK}" = 0 ]; then
      # 进一步区分：TCP 能否建立？明文 80 端口是否同样被断开？
      if nc -z -G 5 ${PHYS_IF:+-b "${PHYS_IF}"} "${ip}" 443 2>/dev/null || nc -z -w 5 "${ip}" 443 2>/dev/null; then
        info "TCP 443 可以建立 → 连接在 TLS/数据阶段被断开"
      else
        info "TCP 443 无法建立"
      fi
      probe "${ip}:80 明文 HTTP" "${DIRECT_ARGS[@]}" -H "Host: ${HOST}" "http://${ip}/"
    fi
  done
fi

bold "5. 对照组：同样直连火山的其他域名（${CONTROL_HOST}）"
CONTROL_OK=0
CONTROL_IP=$(public_ips "${CONTROL_HOST}" | head -1)
if [ -n "${CONTROL_IP}" ]; then
  probe "${CONTROL_HOST} (${CONTROL_IP})" "${DIRECT_ARGS[@]}" --resolve "${CONTROL_HOST}:443:${CONTROL_IP}" "https://${CONTROL_HOST}/" && CONTROL_OK=1
else
  bad "无法解析 ${CONTROL_HOST}"
fi

if [ "${SYSTEM_OK}" = 1 ] || [ "${DIRECT_OK}" = 1 ]; then
  bold "附加：用 ${MODEL} 发一次最小对话"
  extra=()
  [ "${SYSTEM_OK}" = 0 ] && extra=("${DIRECT_ARGS[@]}" --resolve "${HOST}:443:$(echo ${REAL_IPS} | awk '{print $1}')")
  resp=$(curl -sS -m 60 "${extra[@]}" "${ARK_BASE_URL}/chat/completions" \
    -H "Authorization: Bearer ${ARK_API_KEY}" -H 'Content-Type: application/json' \
    -d "{\"model\":\"${MODEL}\",\"messages\":[{\"role\":\"user\",\"content\":\"只回复两个字：收到\"}],\"stream\":false}" 2>&1)
  echo "${resp}" | sed "s/${ARK_API_KEY}/***/g" | cut -c1-400 | sed 's/^/    /'
fi

bold "6. 结论"
if [ "${SYSTEM_OK}" = 1 ]; then
  ok "系统路径可用：yanshi 可以直接访问方舟，无需处理。"
elif [ "${DIRECT_OK}" = 1 ]; then
  bad "直连可用、经代理不可用 → 是代理规则问题。"
  info "在代理软件中把 ${HOST}（或 *.volces.com）设为 DIRECT 后重新运行本脚本。"
elif [ "${CONTROL_OK}" = 1 ]; then
  bad "直连火山其他域名正常，唯独方舟的 IP 被断开 → 不是代理问题。"
  info "可能原因：局域网路由器的安全/拦截功能、运营商拦截这些 IP，或方舟侧屏蔽了本出口 IP。"
  info "建议：切换到手机热点后重新运行；若热点下正常，检查路由器设置（如家长控制、安全防护、广告过滤）。"
  info "若热点下仍失败，携带本脚本输出联系火山引擎支持。"
else
  bad "直连火山的其他域名也失败 → 本机直连网络整体异常，或物理网卡选择不对（当前：${PHYS_IF:-无}）。"
  info "可用 PHYS_IF 之外的网卡重试，或暂时关闭代理软件后重新运行。"
fi
rm -f /tmp/ark-diagnose.err
