#!/bin/zsh
P=/Users/dukler/Workspace/workass/.dev/rail-proof; M=m-7c745fa693fa4d4a87d35c0534eb1813; RUN=$1
J() { python3 -I - "$@" <<'PY'
import json,sys
a=sys.argv[1:]; d=dict(machine_id=a[0],operation_id=a[1],label=a[2],cwd='/tmp',command=json.loads(a[3]))
d.update(json.loads(a[4]) if len(a)>4 else {})
print(json.dumps(d))
PY
}
S() { $P/tool workass_start_process_child "$(J $M "$@")" | python3 -I -c "
import sys,json;d=json.loads(sys.stdin.read());print({k:d.get(k) for k in('workId','supervisorPid','survivesRestart','warning','error') if d.get(k) is not None})"; }
S $RUN-hold-00001 "$RUN ok (retiene r1)" '["sleep","20"]' '{"resource":"r1"}'
S $RUN-queued-0001 "$RUN ok (en cola)" '["sleep","5"]' '{"resource":"r1"}'
S $RUN-refused-001 "$RUN rechazado" '["sleep","5"]' '{"inputs":["/nonexistent/input"]}'
S $RUN-timeout-001 "$RUN timed_out" '["sleep","60"]' '{"max_runtime_ms":4000}'
S $RUN-lost-00001 "$RUN perdido" '["sleep","600"]' | tee $P/c5lost
