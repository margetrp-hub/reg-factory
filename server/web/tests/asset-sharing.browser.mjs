import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile,stat,mkdir} from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {chromium} from 'playwright';
const root=fileURLToPath(new URL('../',import.meta.url)),out=path.join(root,'output','asset-sharing');
const profiles=JSON.parse(await readFile(new URL('../../../shared/node-sdk/contracts/asset-profiles.v1.json',import.meta.url),'utf8')).profiles;
const mime={'.html':'text/html','.js':'application/javascript','.css':'text/css','.txt':'text/x-component','.woff2':'font/woff2'};
const server=createServer(async(req,res)=>{try{let f=path.resolve(root,'out','.'+new URL(req.url,'http://localhost').pathname);assert(f.startsWith(path.join(root,'out')+path.sep));if((await stat(f)).isDirectory())f=path.join(f,'index.html');res.setHeader('Content-Type',mime[path.extname(f)]||'application/octet-stream');res.end(await readFile(f));}catch{res.writeHead(404);res.end();}});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));const origin=`http://127.0.0.1:${server.address().port}`;
const browser=await chromium.launch({headless:true}),page=await browser.newPage({viewport:{width:1440,height:1000}});
const records=[],submissions=[],errors=[],byRef=new Map();let lose=true,bulkFailure=true,bulkLost=true,releaseMail,mailStarted;
const mailSeen=new Promise(resolve=>mailStarted=resolve),mailGate=new Promise(resolve=>releaseMail=resolve);
const candidates=[{ref:'one',email:'one@example.test',platform:'chatgpt',status:'active'},
 ...Array.from({length:27},(_,i)=>({ref:'a-'+String(i+1).padStart(2,'0'),email:'account-'+(i+1)+'@example.test',platform:i%2?'chatgpt':'claude',status:'active'})),
 ...Array.from({length:3},(_,i)=>({ref:'mail:mail-'+i+'@example.test',email:'mail-'+i+'@example.test',platform:'mail',status:'active'}))];
page.setDefaultTimeout(45000);page.setDefaultNavigationTimeout(90000);
page.on('pageerror',e=>errors.push(e.message));
await page.route('**/*',async route=>{
 const req=route.request(),url=new URL(req.url()),p=url.pathname;assert.equal(url.origin,origin);if(!p.startsWith('/api/'))return route.continue();
 const json=v=>route.fulfill({json:v});
 if(p==='/api/session')return json({ok:true,user:'synthetic-admin'});
 if(p==='/api/system')return json({version:'test',task_stats:{},memory:{current_mb:1,max_mb:100,used_pct:1}});
 if(p==='/api/sharing'){
  if(url.searchParams.get('view')==='history')return json({records});
  if(url.searchParams.get('view')==='received')return json({received:[{task_id:'received-one',account_id:'central-asset',revision:1,received_at:'2026-11-13T00:00:00Z'}]});
  const type=url.searchParams.get('type')||'all',q=url.searchParams.get('q')||'',offset=Number(url.searchParams.get('offset')||0);
  const filtered=candidates.filter(row=>(type==='all'||(type==='mailbox')===row.ref.startsWith('mail:'))&&row.email.includes(q)&&!records.some(r=>r.ref===row.ref&&r.status==='transferred'));
  return json({records,candidates:filtered.slice(offset,offset+25),total:filtered.length,profiles});
 }
 if(p==='/api/sharing/preview'){const b=req.postDataJSON();return json({asset:{email:candidates.find(row=>row.ref===b.ref)?.email},present:['email','password'],missing:[]});}
 if(p==='/api/sharing/consent'){
  const b=req.postDataJSON();submissions.push(b);assert.deepEqual(b.options.fields,{});assert.equal(b.options.include_materials,false);assert.equal(b.options.include_credentials,false);
  if(byRef.has(b.ref))assert.deepEqual(b,byRef.get(b.ref));else byRef.set(b.ref,b);
  if(b.ref==='a-02'&&bulkFailure){bulkFailure=false;return route.fulfill({status:409,json:{error:'synthetic explicit refusal'}});}
  if(b.ref==='mail:mail-0@example.test'&&!records.some(r=>r.ref===b.ref)){mailStarted();await mailGate;}
  let record=records.find(r=>r.ref===b.ref);if(!record){record={ref:b.ref,status:'pending',pending:true,request_id:b.request_id,center_id:'center'};records.push(record);}
  if(b.ref==='one'&&lose){lose=false;return route.fulfill({status:408,json:{error:'synthetic timeout after save'}});}
  if(b.ref==='a-01'&&bulkLost){bulkLost=false;return route.fulfill({status:503,json:{error:'synthetic lost batch ACK'}});}
  return json(record);
 }
 if(p==='/api/sharing/sync'){const b=req.postDataJSON(),record=records.find(r=>r.ref===b.ref);Object.assign(record,{status:'transferred',pending:false,last_sync:'2026-11-13T00:00:00Z'});return json(record);}
 throw new Error('Unexpected '+p);
});
const visit=async(path,title)=>{await page.goto(origin+path);await page.getByRole('heading',{name:title,exact:true}).waitFor();await page.getByRole('table',{name:title,exact:true}).waitFor();};
try{
 await mkdir(out,{recursive:true});await visit('/nodes/sharing/','节点现有资料');
 assert.equal(await page.getByRole('table').count(),1);assert.equal(submissions.length,0);
 const single=page.getByRole('dialog',{name:'移交资料',exact:true});
 await page.getByRole('row').filter({hasText:'one@example.test'}).getByRole('button',{name:'移交',exact:true}).click();
 await single.getByLabel('邮箱密码',{exact:true}).waitFor();assert.equal(await single.locator('[required]').count(),0);
 await page.screenshot({path:path.join(out,'desktop-form.png'),fullPage:true});
 await single.getByLabel('账号 / 邮箱资料',{exact:true}).uncheck();await single.getByRole('checkbox',{name:'凭据 JSON',exact:true}).uncheck();
 await single.getByRole('button',{name:'确认移交',exact:true}).click();await single.getByRole('button',{name:'重试原请求',exact:true}).click();await single.waitFor({state:'hidden'});
 await page.getByRole('link',{name:'移交记录',exact:true}).click();await page.getByRole('heading',{name:'移交记录',exact:true}).waitFor();
 assert.equal(await page.getByRole('table').count(),1);
 await page.getByRole('button',{name:'重试原移交',exact:true}).click();await page.getByText('已移交',{exact:true}).waitFor();
 await page.screenshot({path:path.join(out,'history.png'),fullPage:true});
 await page.getByRole('link',{name:'收到的资料',exact:true}).click();await page.getByText('central-asset',{exact:true}).waitFor();
 assert.equal(await page.getByRole('table').count(),1);await page.screenshot({path:path.join(out,'received.png'),fullPage:true});
 await visit('/nodes/sharing/','节点现有资料');await page.getByText('共 30 项 · 已选 0 项',{exact:true}).waitFor();
 await page.getByLabel('资料分类',{exact:true}).selectOption('mailbox');await page.getByText('共 3 项 · 已选 0 项',{exact:true}).waitFor();
 await page.getByLabel('选择 mail-0@example.test',{exact:true}).check();await page.getByText('共 3 项 · 已选 1 项',{exact:true}).waitFor();
 await page.getByLabel('全选本页',{exact:true}).check();await page.getByText('共 3 项 · 已选 3 项',{exact:true}).waitFor();
 assert.equal(submissions.length,2);
 const transferButton=page.getByRole('button',{name:'批量移交 (3)',exact:true}),desktopButton=await transferButton.boundingBox();
 await page.emulateMedia({reducedMotion:'reduce'});await page.setViewportSize({width:390,height:844});
 const mobileButton=await transferButton.boundingBox();assert(mobileButton.height<desktopButton.height&&mobileButton.width<desktopButton.width,'mobile buttons shrink');
 await page.waitForFunction(()=>document.querySelector('aside').getBoundingClientRect().right<=0);
 await page.screenshot({path:path.join(out,'mobile.png'),fullPage:true});
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
 await page.getByRole('row').filter({hasText:'mail-0@example.test'}).getByRole('button',{name:'移交',exact:true}).click();
 await single.getByLabel('邮箱密码',{exact:true}).waitFor();await page.screenshot({path:path.join(out,'mobile-form.png'),fullPage:true});await single.getByRole('button',{name:'关闭',exact:true}).click();await single.waitFor({state:'hidden'});
 await page.setViewportSize({width:1440,height:1000});
 await page.getByLabel('资料分类',{exact:true}).selectOption('account');await page.getByText('共 27 项 · 已选 0 项',{exact:true}).waitFor();
 await page.getByLabel('全选本页',{exact:true}).check();await page.getByText('共 27 项 · 已选 25 项',{exact:true}).waitFor();
 await page.getByRole('button',{name:'下一页',exact:true}).click();await page.getByText('第 2 页',{exact:true}).waitFor();
 await page.getByRole('button',{name:'选择筛选下全部',exact:true}).click();await page.getByText('共 27 项 · 已选 27 项',{exact:true}).waitFor();
 assert.equal(submissions.length,2);
 assert.equal(await page.getByRole('table',{name:'节点现有资料',exact:true}).locator('tbody td').first().evaluate(el=>getComputedStyle(el).padding),'6px 12px');
 await page.screenshot({path:path.join(out,'desktop.png'),fullPage:true});
 await page.getByRole('button',{name:'批量移交 (27)',exact:true}).click();
 const batch=page.getByRole('dialog',{name:'批量移交',exact:true});
 await batch.getByLabel('账号 / 邮箱资料',{exact:true}).uncheck();await batch.getByRole('checkbox',{name:'凭据 JSON',exact:true}).uncheck();
 await batch.getByRole('button',{name:'确认批量移交',exact:true}).click();
 await batch.getByText('共 27 项 · 已提交 25 项 · 未确认 1 项 · 失败 1 项',{exact:true}).waitFor();
 await page.screenshot({path:path.join(out,'batch-recovery.png'),fullPage:true});
 await batch.getByRole('button',{name:'重试未完成项',exact:true}).click();await batch.getByText('共 27 项 · 已提交 27 项 · 未确认 0 项 · 失败 0 项',{exact:true}).waitFor();
 assert.equal(submissions.filter(row=>row.ref==='a-01').length,2);assert.equal(submissions.filter(row=>row.ref==='a-02').length,2);assert.equal(submissions.filter(row=>row.ref==='a-03').length,1);
 await batch.getByRole('button',{name:'关闭',exact:true}).click();await batch.waitFor({state:'hidden'});
 await page.getByLabel('资料分类',{exact:true}).selectOption('mailbox');await page.getByText('共 3 项 · 已选 0 项',{exact:true}).waitFor();
 await page.getByLabel('全选本页',{exact:true}).check();await page.getByRole('button',{name:'批量移交 (3)',exact:true}).click();
 await batch.getByLabel('账号 / 邮箱资料',{exact:true}).uncheck();await batch.getByRole('checkbox',{name:'凭据 JSON',exact:true}).uncheck();
 await batch.getByRole('button',{name:'确认批量移交',exact:true}).click();await mailSeen;
 await batch.getByRole('button',{name:'停止后续提交',exact:true}).click();releaseMail();
 await batch.getByRole('button',{name:'重试未完成项',exact:true}).waitFor();
 assert.equal(submissions.filter(row=>row.ref.startsWith('mail:')).length,1);
 await batch.getByRole('button',{name:'重试未完成项',exact:true}).click();await batch.getByText('共 3 项 · 已提交 3 项 · 未确认 0 项 · 失败 0 项',{exact:true}).waitFor();
 assert.equal(submissions.filter(row=>row.ref.startsWith('mail:')).length,3);
 assert.deepEqual(errors,[]);
 console.log(JSON.stringify({passed:true,checks:['three independent table pages','category and checkbox selection','all pages selection without writes','optional single and batch payloads','partial failure continues other items','lost ACK retries original only','stop and resume unsent items','mobile table scroll without page overflow']}));
}catch(error){console.error(error,{errors,body:await page.locator('body').innerText()});await page.screenshot({path:path.join(out,'failure.png'),fullPage:true});throw error;}
finally{releaseMail();await browser.close();await new Promise(resolve=>server.close(resolve));}
