# rail.sh: drive the hidden renderer. shot NAME | js 'expr' | send 'text'
c=http://127.0.0.1:18992
case "$1" in
shot) curl -s "$c/shot?name=$2" ;;
js) curl -s -X POST --data-binary "$2" "$c/eval" ;;
send) python3 -I - "$2" <<'PY' > /tmp/rail-send.js
import json,sys
t=json.dumps(sys.argv[1])
print("(()=>{const a=document.querySelector('.comp textarea');const s=Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set;s.call(a,%s);a.dispatchEvent(new Event('input',{bubbles:true}));return a.value.length})()"%t)
PY
  curl -s -X POST --data-binary @/tmp/rail-send.js "$c/eval"; sleep 0.4
  curl -s -X POST --data-binary "(()=>{const b=[...document.querySelectorAll('.comp button')].pop();b&&b.click();return b&&b.className})()" "$c/eval" ;;
esac
