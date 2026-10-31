import json,sys,re
d=json.load(sys.stdin); ms=d.get('messages') or d
if isinstance(d,dict) and 'error' in d: print(d)
for m in ms if isinstance(ms,list) else []:
    c=str(m.get('content') or m.get('text') or '')
    if m.get('role')=='user':
        lab=re.findall(r'<task-notification>',c)
        s=re.search('<summary>(.*?)</summary>',c,re.S); st=re.search('<status>(.*?)</status>',c)
        print('USER',m.get('at'),'NOTICE' if lab else '',st and st.group(1),'|',(s.group(1) if s else c)[:170].replace('\n',' '))
    else: print('  asst',m.get('at'),m.get('status'),c[:90].replace('\n',' '))
