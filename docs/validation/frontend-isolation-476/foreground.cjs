const fs=require('node:fs');const path=require('node:path');const {spawn}=require('node:child_process');
const [prefix,...args]=process.argv.slice(2);if(!prefix||!args.length)throw new Error('Pass log prefix and command argv');
const out=fs.openSync(prefix+'.log','w');const started=Date.now();const signals=[];
const child=spawn(args[0],args.slice(1),{detached:true,stdio:['ignore',out,out],env:process.env});
process.on('SIGTERM',()=>{signals.push('SIGTERM');if(child.exitCode===null)child.kill('SIGTERM');});
process.on('SIGINT',()=>{signals.push('SIGINT');if(child.exitCode===null)child.kill('SIGINT');});
child.on('error',e=>{console.error(e);process.exitCode=1;});
child.on('exit',(code,signal)=>{
 fs.closeSync(out);const result={command:args,child:{code,signal},supervisor:{code:code===0?0:1,signals},elapsed_ms:Date.now()-started};
 fs.writeFileSync(prefix+'.exit.json',JSON.stringify(result,null,2)+'\n');console.log(JSON.stringify(result));process.exitCode=code===0?0:1;
});
