import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, mkdir, stat, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import { chromium } from 'playwright';

// Exercise the real exported application; every API and SSE reply is synthetic.
const root = fileURLToPath(new URL('../', import.meta.url));
const output = path.join(root, 'output', 'modes');
const schema = JSON.parse(await readFile(path.join(root, '../../deploy/config.schema.json'), 'utf8'));
const scripts = JSON.parse(await readFile(path.join(root, '../../deploy/scripts.schema.json'), 'utf8'));
const catalogue = JSON.parse(await readFile(path.join(root, 'lib/generated-catalog.json'), 'utf8'));
const fields = new Map(schema.flatMap(group => group.items.map(item => [item.key, item])));
const values = { FINGERPRINT_BROWSER: 'bitbrowser', BITBROWSER_API: 'http://browser.example.test',
  ICLOUD_MAIL_API_BASE: 'https://previous.example.test',
  CLOAK_LOCALE: 'en-US', ROXY_API_TOKEN: 'synthetic-roxy-secret', CHATGPT_EMAIL_PROVIDER: 'pool',
  TEMP_EMAIL_PROVIDER: 'yyds', OUTLOOK_GRAPH_RECOVERY_PROVIDER: 'yyds', REG_FACTORY_LIVENESS_ENABLED: '0',
  PRIVATE_EXTRA_OPTION: 'preserved', PROXY_MODE: 'clash_auto' };
const initialKeys = Object.keys(values);
const deployed = {REG_FACTORY_WORK_ORIGIN:'http://work.example.test:18099',REG_FACTORY_ASSET_CENTER_ID:'production-center-test',REG_FACTORY_NODE_ID:'sg-test'};
const requests = [], problems = [], tasks = new Map();
const mailSources = [{id:1,name:'Microsoft test inbox',transport:'graph',host:'',port:993,
  username:'owner@example.test',folder:'INBOX',enabled:true,idle:false,intervalSeconds:10,
  heartbeatSeconds:60,clientID:'test-client',refreshTokenSet:true,passwordSet:false,runtime:'running'}];
const mailboxes = [], sourceSaves = [], platformStates = new Map();
let mailStateReject = false;
let readFailure = true, saveMode = 'ok', taskMode = 'ok', saves = 0, starts = 0;
const mime = { '.html': 'text/html', '.js': 'application/javascript', '.css': 'text/css', '.json': 'application/json', '.txt': 'text/x-component', '.woff2': 'font/woff2', '.svg': 'image/svg+xml' };
const server = createServer(async (req, res) => {
  try {
    const pathname = decodeURIComponent(new URL(req.url, 'http://localhost').pathname);
    let file = path.resolve(root, 'out', '.' + pathname);
    assert(file.startsWith(path.resolve(root, 'out') + path.sep) || file === path.resolve(root, 'out'));
    if ((await stat(file)).isDirectory()) file = path.join(file, 'index.html');
    res.setHeader('Content-Type', mime[path.extname(file)] || 'application/octet-stream');
    res.end(await readFile(file));
  } catch { res.writeHead(404); res.end('Not found'); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await chromium.launch({ headless: true });
const context = await browser.newContext({ viewport: {width: 1440, height: 1000} });
const page = await context.newPage();
page.setDefaultTimeout(15000);
page.on('pageerror', err => problems.push(err.message));
page.on('dialog', dialog => dialog.accept());
await context.route('**/*', async route => {
  const request = route.request(), url = new URL(request.url()), endpoint = url.pathname;
  if (url.origin !== origin) { problems.push(`External request: ${url.origin}`); return route.abort(); }
  if (!endpoint.startsWith('/api/')) return route.continue();
  requests.push([request.method(), endpoint]);
  const json = data => route.fulfill({json: data});
  const fail = (status, error) => route.fulfill({status, json: {error}});
  if (endpoint === '/api/session') return json({ok:true, user:'synthetic-admin', actor:'admin', anon:false});
  if (endpoint === '/api/system') return json({version:'local', uptime_sec:10, memory:{current_mb:1, max_mb:100, used_pct:1}, task_stats:{}, slots_total:3, slots_used:0});
  if (endpoint === '/api/config') {
    if (request.method() === 'PUT') {
      saves++;
      if (saveMode === 'reject') { saveMode = 'ok'; return fail(422, 'synthetic validation rejection'); }
      Object.assign(values, request.postDataJSON().updates);
      if (saveMode === 'lost') { saveMode = 'ok'; return route.abort('failed'); }
      return json({ok:true, changed:Object.keys(request.postDataJSON().updates)});
    }
    if (readFailure) { readFailure = false; return fail(503, 'synthetic read failure'); }
    const groups = Object.fromEntries(schema.map(group => [group.group, group.items.map(item => {
      const set=Object.hasOwn(values,item.key), saved=values[item.key] || '';
      const seed=deployed[item.key];
      const effective=!set && seed!==undefined ? seed : (!set || item.empty_default && !saved.trim()) ? String(item.default ?? '') : saved;
      return {...item, value:!set && !item.secret ? effective : saved, line:set ? 1 : 0, area:group.area, effective_source:!set && seed!==undefined ? 'deployment' : !set || item.empty_default && !saved.trim() ? 'default' : 'file', effective_value:item.secret ? '' : effective, effective_set:!!effective};
    })]));
    groups.custom = [{key:'PRIVATE_EXTRA_OPTION', label:'自定义参数', value:values.PRIVATE_EXTRA_OPTION, line:1, secret:false, area:'custom'}];
    return json({path:'/synthetic/.env', groups});
  }
  if (endpoint === '/api/scripts') return json({scripts, count:scripts.length});
  if (endpoint === '/api/apps/mail/sources') {
    if (request.method() === 'POST') {
      const body = request.postDataJSON(); sourceSaves.push(body);
      const source = mailSources.find(item=>item.id===body.id);
      Object.assign(source, body, {refreshTokenSet:source.refreshTokenSet || !!body.refreshToken});
      delete source.refreshToken; delete source.password;
      return json({ok:true,data:source});
    }
    return json({ok:true, data:{sources:mailSources}});
  }
  if (endpoint === '/api/apps/mail/pool') return json({ok:true, data:{items:mailboxes,total:mailboxes.length}});
  if (endpoint === '/api/apps/mail/pool/availability') return json({ok:true,data:{
    platforms:[...platformStates].map(([platform,state])=>({platform,state})),parentId:'',parentState:'active'}});
  if (endpoint === '/api/apps/mail/pool/platform-state') {
    if (mailStateReject) { mailStateReject=false; return route.fulfill({status:409,json:{ok:false,error:{message:'synthetic state save failure'}}}); }
    const body=request.postDataJSON(); platformStates.set(body.platform,body.state);
    return json({ok:true,data:{saved:true}});
  }
  if (endpoint === '/api/accounts/summary') return json({platforms:[{platform:'chatgpt',total:3,available:2,abnormal:1,unknown:0,used:0,leased:0,source_count:3}]});
  if (endpoint === '/api/tasks' && request.method() === 'POST') {
    starts++;
    if (taskMode === 'reject') {taskMode='ok';return fail(422,'synthetic task rejection');}
    const body = request.postDataJSON(), id = 'request-' + body.request_id;
    tasks.set(id, {id,script:body.script,title:scripts.find(item => item.id === body.script).title,args_json:JSON.stringify(body.args),status:'succeeded',exit_code:0,created_at:new Date().toISOString(),finished_at:new Date().toISOString()});
    if (taskMode === 'lost') {taskMode='ok';return route.abort('failed');}
    return json({id,status:'queued'});
  }
  if (endpoint === '/api/tasks') return json({tasks:[...tasks.values()],count:tasks.size,total:tasks.size,stats:{succeeded:tasks.size},offset:0,limit:25});
  if (endpoint.startsWith('/api/tasks/')) {
    const id = endpoint.split('/')[3];
    if (endpoint.endsWith('/stream')) return route.fulfill({contentType:'text/event-stream',body:
      `event: log\nid: 1\ndata: ${JSON.stringify({id:1,task_id:id,ts:new Date().toISOString(),stream:'stdout',line:'synthetic saved result'})}\n\nevent: done\ndata: {"status":"succeeded","exit_code":0}\n\n`});
    return tasks.has(id) ? json(tasks.get(id)) : fail(404,'task not found');
  }
  problems.push(`Unexpected API: ${request.method()} ${endpoint}`);
  return fail(500,'unexpected synthetic endpoint');
});
const button = name => page.getByRole('button', {name,exact:true});
const input = key => page.getByLabel(fields.get(key)?.label || key, {exact:true});
const value = key => input(key).inputValue();
const save = () => page.getByRole('button', {name:/^保存全部修改/}).click();
const loaded = () => button('查看全部模式配置').waitFor();
try {
  await mkdir(output, {recursive:true});
  await page.goto(origin + '/settings/?category=browser');
  await page.getByText(/配置读取失败：synthetic read failure/).waitFor();
  await button('重新读取').click(); await loaded();
  assert.equal(await value('FINGERPRINT_BROWSER'), 'bitbrowser');
  await input('FINGERPRINT_BROWSER').selectOption('cloak');
  await input('CLOAK_LOCALE').fill('zh-CN');
  await input('FINGERPRINT_BROWSER').selectOption('roxy');
  await input('ROXY_API_TOKEN').fill('synthetic-new-roxy');
  await input('FINGERPRINT_BROWSER').selectOption('cloak');
  assert.equal(await value('CLOAK_LOCALE'),'zh-CN');
  await save(); await page.getByRole('button',{name:'保存全部修改',exact:true}).waitFor();
  assert.equal(values.ROXY_API_TOKEN,'synthetic-new-roxy'); assert.equal(values.CLOAK_LOCALE,'zh-CN');
  assert.equal(values.FINGERPRINT_BROWSER,'cloak'); assert.equal(values.REG_FACTORY_LIVENESS_ENABLED,'0');
  await page.reload(); await loaded(); assert.equal(await value('CLOAK_LOCALE'),'zh-CN');
  saveMode='reject'; await input('CLOAK_LOCALE').fill('en-GB'); await save();
  await page.getByText(/保存未确认：synthetic validation rejection/).waitFor();
  assert.equal(await value('CLOAK_LOCALE'),'en-GB'); assert.equal(values.CLOAK_LOCALE,'zh-CN');
  saveMode='lost'; await save(); await page.getByText(/保存未确认：/).waitFor();
  await button('重新读取').click(); await page.getByRole('button',{name:'保存全部修改',exact:true}).waitFor();
  assert.equal(await value('CLOAK_LOCALE'),'en-GB'); assert.equal(saves,3);
  await button('查看全部模式配置').click();
  assert.equal(await input('CLOAK_EXTRA_ARGS').count(),0,'advanced tuning starts collapsed');
  await page.getByRole('button',{name:/^高级参数/}).click();
  for (const field of schema.filter(group => group.area === 'browser').flatMap(group => group.items)) assert.equal(await input(field.key).count(),1,field.key);
  await button('邮箱与取码').click();
  assert.equal(await input('YYDS_API_KEY').count(),1,'two usages share one provider configuration');
  await input('CHATGPT_EMAIL_PROVIDER').selectOption('icmail');
  await save(); await page.getByRole('button',{name:'保存全部修改',exact:true}).waitFor();
  assert.equal(await value('CHATGPT_EMAIL_PROVIDER'),'icmail');
  assert.equal(await page.locator('.sg-config-workspace details').count(),0);
  await button('查看全部模式配置').click();
  await input('ICLOUD_MAIL_API_BASE').fill('');
  await save(); await page.getByRole('button',{name:'保存全部修改',exact:true}).waitFor();
  assert.equal(await input('ICLOUD_MAIL_API_BASE').getAttribute('placeholder'),'默认 https://mail.no-replyca.xyz');
  await input('CHATGPT_EMAIL_PROVIDER').selectOption('icloud'); await input('ICLOUD_MAIL_SERVICE').fill('synthetic-openai');
  await input('CHATGPT_EMAIL_PROVIDER').selectOption('remail'); await input('REMAIL_PROJECT_ID').fill('123');
  await input('CHATGPT_EMAIL_PROVIDER').selectOption('icloud'); assert.equal(await value('ICLOUD_MAIL_SERVICE'),'synthetic-openai');
  await button('浏览器').click(); assert.equal(await value('CLOAK_LOCALE'),'en-GB'); await save();
  await page.getByRole('button',{name:'保存全部修改',exact:true}).waitFor();
  for (const key of initialKeys) assert(Object.hasOwn(values,key),key);
  assert.equal(values.PRIVATE_EXTRA_OPTION,'preserved'); assert.equal(values.REG_FACTORY_LIVENESS_ENABLED,'0');
  await button('短信接码').click(); await page.getByLabel('配置方式',{exact:true}).selectOption('hero');
  await input('HERO_SMS_API_KEY').fill('synthetic-hero');
  await page.getByLabel('配置方式',{exact:true}).selectOption('smsman');
  assert.equal(await input('CODEX_ADDPHONE_ATTEMPTS').count(),1,'SMS retries remain common to providers');
  await page.getByLabel('配置方式',{exact:true}).selectOption('hero');
  assert.equal(await value('HERO_SMS_API_KEY'),'synthetic-hero');
  await save(); await page.getByRole('button',{name:'保存全部修改',exact:true}).waitFor();
  await button('浏览器').click();
  await page.screenshot({path:path.join(output,'config-desktop.png'),fullPage:true});

  // Every script and all of its original arguments remain reachable.
  for (const group of catalogue.groups) {
    await page.goto(origin + `/tasks/group/${group.slug}/`);
    const expected = scripts.filter(script => script.group === group.slug).map(script=>script.id);
    if (!expected.length) continue;
    await page.getByLabel('执行模式').locator('option').first().waitFor({state:'attached'});
    assert.deepEqual(await page.getByLabel('执行模式').locator('option').evaluateAll(options=>options.map(option=>option.value)),expected);
  }
  await page.goto(origin+'/tasks/group/pipeline/');
  const selector = page.getByLabel('执行模式'); await selector.selectOption('run_full_flow');
  const original = scripts.find(item=>item.id==='run_full_flow');
  const arg = original.args.find(item=>item.type==='int'); const label=arg.flag.replace(/^--/,'');
  await page.locator('form:visible').getByLabel(label,{exact:true}).fill('7');
  await selector.selectOption('register_three_platforms');
  const second=scripts.find(item=>item.id==='register_three_platforms');
  const secondArg=second.args.find(item=>item.type==='str' && !/password|token|secret/.test(item.flag));
  await page.locator('form:visible').getByLabel(secondArg.flag.replace(/^--/,''),{exact:true}).fill('synthetic-input');
  await selector.selectOption('run_full_flow');
  assert.equal(await page.locator('form:visible').getByLabel(label,{exact:true}).inputValue(),'7');
  taskMode='reject'; await page.locator('form:visible').getByRole('button',{name:'启动任务',exact:true}).click();
  await page.getByText('synthetic task rejection',{exact:true}).waitFor();
  assert.equal(tasks.size,0);
  taskMode='lost'; await page.locator('form:visible').getByRole('button',{name:'启动任务',exact:true}).click();
  await page.locator('form:visible').getByRole('button',{name:'核对原请求',exact:true}).click();
  await page.getByRole('link',{name:'查看运行与实时日志'}).waitFor();
  assert.equal(starts,2); assert.equal(tasks.size,1,'lost acknowledgement cannot create a second task');
  await selector.selectOption('register_three_platforms');
  assert.equal(await page.locator('form:visible').getByLabel(secondArg.flag.replace(/^--/,''),{exact:true}).inputValue(),'synthetic-input');
  await page.screenshot({path:path.join(output,'task-desktop.png'),fullPage:true});
  await page.getByRole('link',{name:'查看运行与实时日志'}).click();
  await page.getByText('synthetic saved result',{exact:true}).waitFor();
  await page.getByText('成功 · 退出码 0',{exact:true}).waitFor();

  const start=requests.length;
  await page.goto(origin+'/production/'); await page.getByRole('link',{name:/凭据维护/}).waitFor();
  await page.waitForTimeout(16000);
  assert.equal(requests.slice(start).filter(([method,url])=>url==='/api/scripts').length,1,'polling must not reload the mode catalogue');
  assert(requests.slice(start).filter(([method,url])=>url==='/api/tasks').length <= 2,'one summary read, not one per platform');
  await page.screenshot({path:path.join(output,'production-desktop.png'),fullPage:true});
  const beforeMail=requests.length;
  await page.goto(origin+'/materials/mail/'); await page.waitForURL('**/apps/mail/');
  await page.getByRole('button',{name:'导入地址',exact:true}).waitFor();
  await page.getByText('还没有邮箱地址。可先导入地址，也可由收信自动发现。',{exact:true}).waitFor();
  assert.equal(requests.slice(beforeMail).filter(([,url])=>url==='/api/apps/mail/pool').length,1,'legacy mail entry must use one canonical pool view');
  await page.goto(origin+'/apps/mail/sources/');
  await page.getByRole('button',{name:'Microsoft test inbox',exact:true}).click();
  const sourceDialog=page.getByRole('dialog');
  assert.equal(await sourceDialog.getByLabel(/^收信方式/).inputValue(),'graph');
  assert.equal(await sourceDialog.getByLabel(/^邮箱 Refresh Token/).inputValue(),'');
  await sourceDialog.getByLabel('来源名称',{exact:true}).fill('Reauthorized inbox');
  await sourceDialog.getByRole('button',{name:'保存',exact:true}).click();
  await sourceDialog.waitFor({state:'hidden'});
  assert.equal(sourceSaves.at(-1).refreshToken,'','blank save must preserve existing RT');
  assert.equal(mailSources[0].refreshTokenSet,true);
  await page.getByRole('button',{name:'Reauthorized inbox',exact:true}).click();
  await sourceDialog.getByLabel(/^收信方式/).selectOption('pop3');
  assert.equal(await sourceDialog.getByLabel('端口',{exact:true}).inputValue(),'995');
  assert.equal(await sourceDialog.getByLabel(/^邮箱 Refresh Token/).count(),0);
  await sourceDialog.getByLabel(/^收信方式/).selectOption('imap');
  assert.equal(await sourceDialog.getByLabel('端口',{exact:true}).inputValue(),'993');
  await sourceDialog.getByLabel('优先使用实时推送（IDLE）',{exact:true}).waitFor();
  await sourceDialog.getByLabel(/^收信方式/).selectOption('graph');
  assert.equal(await sourceDialog.getByLabel('Client ID',{exact:true}).inputValue(),'test-client');
  await sourceDialog.getByRole('button',{name:'取消',exact:true}).click();
  await sourceDialog.waitFor({state:'hidden'});
  mailboxes.push({email:'person@example.test',sourceId:1,sourceName:'Reauthorized inbox',sourceEnabled:true,
    state:'active',manualState:'active',origin:'imported',total:0,riskHits:0,manual:true,provider:'mail',role:'address',transport:'graph'});
  await page.goto(origin+'/apps/mail/');
  await page.getByRole('button',{name:'person@example.test',exact:true}).click();
  const mailDialog=page.getByRole('dialog');
  await mailDialog.getByLabel(/^应用范围/).locator('option').first().waitFor({state:'attached'});
  assert.deepEqual(await mailDialog.getByLabel(/^应用范围/).locator('option').evaluateAll(options=>options.map(o=>o.value)),catalogue.platforms.map(p=>p.slug));
  await mailDialog.getByLabel(/^应用范围/).selectOption('chatgpt');
  await mailDialog.getByLabel(/^使用状态/).selectOption('banned');
  mailStateReject=true;
  await mailDialog.getByRole('button',{name:'更新使用状态',exact:true}).click();
  await mailDialog.getByText('synthetic state save failure',{exact:true}).waitFor();
  assert.equal(await mailDialog.getByLabel(/^使用状态/).inputValue(),'banned','failed save keeps draft');
  await mailDialog.getByRole('button',{name:'更新使用状态',exact:true}).click();
  await page.waitForFunction(()=>document.querySelector('select') && [...document.querySelectorAll('button')].some(b=>b.textContent==='更新使用状态' && b.disabled));
  assert.equal(platformStates.get('chatgpt'),'banned');
  await mailDialog.getByLabel(/^应用范围/).selectOption('github');
  assert.equal(await mailDialog.getByLabel(/^使用状态/).inputValue(),'unknown','platform restriction stays scoped');
  await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1),'mailbox dialog mobile overflow');
  await page.screenshot({path:path.join(output,'mailbox-mobile.png'),fullPage:true});
  await mailDialog.getByRole('button',{name:'取消',exact:true}).click();
  await mailDialog.waitFor({state:'hidden'});
  await page.setViewportSize({width:1440,height:1000});
  await page.screenshot({path:path.join(output,'mailbox-desktop.png'),fullPage:true});
  await page.goto(origin+'/materials/proxy/'); await page.waitForURL('**/settings/?category=proxy*'); await loaded();
  await page.setViewportSize({width:390,height:844});
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1),'mobile page overflow');
  await page.screenshot({path:path.join(output,'config-mobile.png'),fullPage:true});
  await page.goto(origin+'/nodes/config/');
  await page.getByLabel('配置对象').selectOption({label:'中央资产连接'});
  assert.equal(await value('REG_FACTORY_WORK_ORIGIN'),deployed.REG_FACTORY_WORK_ORIGIN,'inherited actual value must be editable');
  assert.equal(await input('REG_FACTORY_NODE_ID').count(),0,'deployment identity is not an edit control');
  await input('REG_FACTORY_WORK_ORIGIN').fill('http://new-work.example.test:18099');
  await page.getByRole('button',{name:'生成资产通信密钥',exact:true}).click();
  const firstKey=await value('REG_FACTORY_ASSET_KEY');
  assert.equal(Buffer.from(firstKey,'base64url').length,32);
  await page.getByRole('button',{name:'生成结果暂存密钥',exact:true}).click();
  assert.notEqual(firstKey,await value('REG_FACTORY_ASSET_JOURNAL_KEY'));
  await save(); await page.getByRole('button',{name:'保存全部修改',exact:true}).waitFor();
  await page.reload(); await page.getByLabel('配置对象').selectOption({label:'中央资产连接'});
  assert.equal(await value('REG_FACTORY_WORK_ORIGIN'),'http://new-work.example.test:18099');
  assert.equal(await value('REG_FACTORY_ASSET_KEY'),firstKey);
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1),'node configuration mobile overflow');
  await page.screenshot({path:path.join(output,'node-config-mobile.png'),fullPage:true});
  await page.setViewportSize({width:1440,height:1000});
  await page.screenshot({path:path.join(output,'node-config-desktop.png'),fullPage:true});
  await page.goto(origin+'/tasks/group/pipeline/'); await page.getByLabel('执行模式').waitFor();
  assert(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth+1),'mobile mode overflow');
  assert.deepEqual(problems,[]);
  console.log(JSON.stringify({result:'passed',scripts:scripts.length,parameters:scripts.reduce((n,item)=>n+item.args.length,0),configurationFields:fields.size,productionWrites:0,checks:['config read retry','mode draft retention','save and reload','rejected save','lost save recovery','all original modes','rejected task','lost task recovery','SSE terminal result','summary request counts','old entry redirects','mail protocol selection','blank RT save','six platform mailbox states','mail state failure recovery','mobile layout']},null,2));
} catch (error) {
  await page.screenshot({path:path.join(output,'failure.png'),fullPage:true}).catch(()=>{});
  await writeFile(path.join(output,'failure.json'),JSON.stringify({error:String(error),url:page.url(),requests,problems},null,2));
  throw error;
} finally { await browser.close(); await new Promise(resolve=>server.close(resolve)); }
