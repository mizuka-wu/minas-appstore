#!/bin/bash
set -u
cd "$(dirname "$0")"
export PATH="$HOME/.local/bin:$PATH"
go build -o dist/store-mac . || exit 1
PORT=18407
./dist/store-mac \
  -catalog ./catalog/catalog.json \
  -uid 0 -nasip 127.0.0.1 -addr 127.0.0.1:$PORT \
  -dry-run -assets ./dist-assets > dist-smoke.log 2>&1 &
SRV=$!
sleep 1
B="http://127.0.0.1:$PORT"
TID() { python3 -c "import sys,json;print(json.load(sys.stdin)['taskId'])"; }
SHOW() { python3 -c "import sys,json;t=json.load(sys.stdin);print('state:',t['state'],'| err:',t.get('error',''))"; }

echo "== 1) 空必填参数应被拒 =="
T=$(curl -s -X POST "$B/api/action" -H 'Content-Type: application/json' \
  -d '{"id":"cftunnel","action":"install","params":{}}' | TID)
sleep 2
curl -s "$B/api/task?id=$T" | SHOW

echo "== 2) 带参数安装：命令拼装 + 泄漏检查 =="
SECRET="sekret-...nly"
T=$(curl -s -X POST "$B/api/action" -H 'Content-Type: application/json' \
  -d "{\"id\":\"cftunnel\",\"action\":\"install\",\"params\":{\"CF_TUNNEL_TOKEN\":\"$SECRET\"}}" | TID)
sleep 7
curl -s "$B/api/task?id=$T" > dist-task.json
python3 - "$SECRET" <<'PY'
import json,sys
sec=sys.argv[1]
t=json.load(open('dist-task.json'))
run=[l for l in t['log'] if ' docker run ' in l]
print('state:',t['state'])
print(run[0] if run else '(no run line)')
blob=json.dumps(t)+open('dist-smoke.log').read()
print('明文泄漏:', 'FAIL' if sec in blob else 'OK 无明文')
PY

echo "== 3) 更新走同一参数校验 =="
T=$(curl -s -X POST "$B/api/action" -H 'Content-Type: application/json' \
  -d '{"id":"cftunnel","action":"update","params":{}}' | TID)
sleep 2
curl -s "$B/api/task?id=$T" | SHOW

kill $SRV 2>/dev/null
echo SCRIPT_DONE
