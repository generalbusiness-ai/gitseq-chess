import os,json,tempfile,pathlib,subprocess
root=pathlib.Path('/var/folders/2x/wylr59t17ds36l1l7ng25y7w0000gn/T/chess-fc3-planner-7hy7_26b').resolve()
scratch=pathlib.Path(tempfile.mkdtemp(prefix='chess-agent-omissions-'))
env={k:v for k,v in os.environ.items() if not k.startswith('GIT_')}
cases=[]
source=(root/'cmd/chess/service_actions.go').read_text()
needle='prepared, err := view.prepareReplay(act)'
assert source.count(needle)==1
cases.append(('replay-position-guard',{'cmd/chess/service_actions.go':source.replace(needle,'if input.Action.Predecessor != "" {\n game, ok := p.GameByID(input.Action.Game)\n if !ok || game.LastMove != input.Action.Predecessor { http.Error(w, "stale replay mutant", 409); return }\n}\n'+needle)},'TestAgentProcessesPlayAndRecoverAcrossServiceAndAdapterRestart','automatic exact replay'))
source=(root/'cmd/chess/main.go').read_text()
route='\tif common.server != "" {\n\t\treturn callAgentTool(ctx, common, params)\n\t}\n'
guard='\tif flags.server != "" {\n\t\treturn nil, nil, errors.New("server mode cannot open a local writer")\n\t}\n'
assert source.count(route)==1 and source.count(guard)==1
cases.append(('no-local-writer-routing',{'cmd/chess/main.go':source.replace(route,'').replace(guard,'')},'TestAgentDestinationRefusesFallbackAndUnsafeOrigins','fell back from unavailable service'))
summary=[]
for name,files,test,expected in cases:
 folder=scratch/name;folder.mkdir()
 replace={}
 for path,text in files.items():
  dest=folder/pathlib.Path(path).name;dest.write_text(text);replace[str(root/path)]=str(dest)
 overlay=folder/'overlay.json';overlay.write_text(json.dumps({'Replace':replace}))
 command=['go','test','-race','-overlay',str(overlay),'./cmd/chess','-run','^'+test+'$','-count=1','-v']
 result=subprocess.run(command,cwd=root,env=env,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True)
 (folder/'test.log').write_text(result.stdout)
 passed=result.returncode!=0 and expected in result.stdout and '[build failed]' not in result.stdout
 summary.append({'control':name,'expected_test_failure_observed':passed,'exit':result.returncode,'test':test,'log':str(folder/'test.log')})
 print(name,passed,flush=True)
pathlib.Path('/tmp/chess-fc3-planner-omissions-summary.json').write_text(json.dumps({'scratch':str(scratch),'controls':summary},indent=2))
raise SystemExit(0 if all(x['expected_test_failure_observed'] for x in summary) else 1)
