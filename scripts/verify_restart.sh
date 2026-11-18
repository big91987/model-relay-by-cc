#!/bin/bash
# 验收：有在途请求时“杀进程”重启，通过正式恢复路径核对账务（使用受控假上游，不产生真实费用）。
# 用法：scripts/verify_restart.sh   （需要先 go build -o bin/gateway ./cmd/gateway && go build -o bin/mockupstream ./cmd/mockupstream）
set -euo pipefail
cd "$(dirname "$0")/.."
W=$(mktemp -d); ROOT=$(pwd)
GW=http://127.0.0.1:3911
export SQLITE_PATH=$W/restart.sqlite ADDR=127.0.0.1:3911 MOCK_ADDR=127.0.0.1:3912 UPSTREAM_TIMEOUT_SECONDS=120
# 不加载真实上游凭证：只连接假上游
env -u DEEPSEEK_API_KEY PROVIDERS_ENV_FILE= ./bin/mockupstream >"$W/mock.log" 2>&1 & MOCK=$!
start_gw() { (cd "$W" && exec "$ROOT/${GATEWAY_BIN:-bin/gateway}") >>"$W/gw.log" 2>&1 & GWPID=$!; for i in $(seq 50); do curl -fsS $GW/healthz >/dev/null 2>&1 && return; sleep 0.1; done; echo "网关启动失败"; exit 1; }
cleanup() { kill -9 $MOCK $GWPID 2>/dev/null || true; rm -rf "$W"; }
trap cleanup EXIT
j() { python3 -I -c "import sys,json; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
api() { curl -sS -b "$W/cj" -H 'Content-Type: application/json' "$@"; }
start_gw
curl -sS -c "$W/cj" -X POST $GW/api/login -H 'Content-Type: application/json' -d '{"username":"admin","password":"admin"}' >/dev/null
api -X POST $GW/api/channels -d '{"name":"假上游","type":"openai","base_url":"http://127.0.0.1:3912","api_key":"x","status":1,"models":[{"model":"mock-chat","upstream_model":"mock-chat"}]}' >/dev/null
api -X PUT $GW/api/models -d '{"name":"mock-chat","kind":"chat","billing_mode":"token","price_in":1000000,"price_out":2000000,"priced":true,"supports_temperature":true}' >/dev/null
KEY=$(api -X POST $GW/api/keys -d '{"name":"重启验证","quota_limit":1000000}' | j "d['key']")
state() { api $GW/api/keys | j "'已用=%d 微元, 占用=%d 微元' % (d['items'][0]['used_cost'], d['items'][0]['reserved_cost'])"; }
chat() { curl -sS -o /dev/null -w '%{http_code}' $GW/v1/chat/completions -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d "$1"; }

echo "1) 先完成一次正常调用（输入 100、输出 50 Token，应计 200 微元）：HTTP $(chat '{"model":"mock-chat","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"mock":{"prompt_tokens":100,"completion_tokens":50}}')"
echo "   $(state)"
echo "2) 发起一个上游迟迟不返回的在途请求，然后 kill -9 网关进程"
chat '{"model":"mock-chat","max_tokens":100,"messages":[{"role":"user","content":"hang"}],"mock":{"hang":true}}' >/dev/null 2>&1 &
sleep 1
echo "   崩溃前：$(state)"
kill -9 $GWPID; wait $GWPID 2>/dev/null || true
echo "3) 重启网关（同一个 SQLite 文件），启动时自动执行恢复"
start_gw
curl -sS -c "$W/cj" -X POST $GW/api/login -H 'Content-Type: application/json' -d '{"username":"admin","password":"admin"}' >/dev/null
echo "   重启后：$(state)   ← 已结算费用未丢失；在途预留转为“待核实”，继续占用额度"
api $GW/api/reservations | j "'   待核实预留 %d 笔：请求 %s，占用 %d 微元，已发往上游=%s；说明：%s' % (len(d['items']), d['items'][0]['request_id'], d['items'][0]['amount'], d['items'][0]['dispatched'], d['items'][0]['note'])"
RID=$(api $GW/api/reservations | j "d['items'][0]['id']")
echo "4) 管理员通过正式接口核实：按实际费用 30 微元结算"
api -X POST $GW/api/reservations/$RID/resolve -d '{"action":"settle","amount":30,"note":"对照上游账单"}' | j "'   结果: %s' % d"
echo "   核实后：$(state)   ← 期望 已用=230，占用=0"
echo "5) 核实后 Key 可继续正常使用：HTTP $(chat '{"model":"mock-chat","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}')"
