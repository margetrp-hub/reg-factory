"use client";

import {useCallback,useEffect,useRef,useState} from "react";
import {request,ApiError} from "@/lib/api";
import {Drawer} from "@/components/drawer";

type Row={ref:string;email?:string;platform?:string;kind?:string;status?:string;error?:string;last_sync?:string;request_id?:string};
type Received={task_id:string;account_id:string;revision:number;received_at:string};
type Listing={records:Row[];candidates:Row[];total:number;received:Received[]};
type Preview={asset:{email?:string};present:string[]};
type ShareRequest={ref:string;request_id:string;options:unknown};
type BatchItem={row:Row;request:ShareRequest;state:"ready"|"sending"|"submitted"|"failed"|"unknown";error?:string};
type View="current"|"history"|"received";
const titles:Record<View,string>={current:"节点现有资料",history:"移交记录",received:"收到的资料"};
const category=(row:Row)=>row.ref.startsWith("mail:")?"邮箱":"账号";
const eligible=(row:Row,records:Row[])=>!records.some(r=>r.ref===row.ref&&r.status!=="rejected");
const fieldsList:[string,string][]=[
 ["registered_platforms","已注册平台"],["email","邮箱"],["email_account","邮箱账号"],["email_password","邮箱密码"],["email_2fa","邮箱 2FA"],
 ["platform_account","平台账号"],["platform_password","平台密码"],["platform_2fa","平台 2FA"],["sms_phone","接码手机号"],["status","状态"]];
const stateNames:Record<string,string>={pending:"等待接收",received:"接收已确认",transferred:"已移交",rejected:"未接收",unreadable:"原记录无法读取"};
const stamp=(v?:string)=>v?new Date(v).toLocaleString("zh-CN",{hour12:false}):"—";
const send=<T,>(url:string,body:unknown)=>request<T>(url,{method:"POST",body:JSON.stringify(body)});

export default function AssetSharing({view}:{view:View}){
 const [type,setType]=useState("all"),[search,setSearch]=useState(""),[query,setQuery]=useState(""),[offset,setOffset]=useState(0);
 const [data,setData]=useState<Listing>({records:[],candidates:[],total:0,received:[]}),[error,setError]=useState(""),[busy,setBusy]=useState(false);
 const [selected,setSelected]=useState<Row|null>(null),[preview,setPreview]=useState<Preview|null>(null),[fields,setFields]=useState<Record<string,string>>({});
 const [materials,setMaterials]=useState(true),[credentials,setCredentials]=useState(true),[sms,setSms]=useState(""),[json,setJson]=useState("");
 const [saving,setSaving]=useState(false),[uncertain,setUncertain]=useState(false),[formError,setFormError]=useState("");
 const [checked,setChecked]=useState<Record<string,Row>>({}),[selecting,setSelecting]=useState(false);
 const [batchOpen,setBatchOpen]=useState(false),[batch,setBatch]=useState<BatchItem[]>([]),[batchBusy,setBatchBusy]=useState(false);
 const [batchMaterials,setBatchMaterials]=useState(true),[batchCredentials,setBatchCredentials]=useState(true);
 const original=useRef<ShareRequest|null>(null),sending=useRef(false),sequence=useRef(0),previewSeq=useRef(0),selectionSeq=useRef(0);
 const batchRequests=useRef<BatchItem[]>([]),batchRunning=useRef(false),stopBatch=useRef(false);
 const listURL=(start:number)=>`/api/sharing?view=${view}&type=${type}&q=${encodeURIComponent(query)}&offset=${start}`;
 const load=useCallback(async()=>{
  const seq=++sequence.current;setBusy(true);
  try{const v=await request<Partial<Listing>>(`/api/sharing?view=${view}&type=${type}&q=${encodeURIComponent(query)}&offset=${offset}`);if(seq===sequence.current){setData({records:[],candidates:[],total:0,received:[],...v});setError("");}}
  catch(e){if(seq===sequence.current)setError((e as Error).message);}finally{if(seq===sequence.current)setBusy(false);}
 },[view,type,query,offset]);
 useEffect(()=>{void load();const timer=setInterval(()=>void load(),10000);return()=>{clearInterval(timer);++sequence.current;};},[load]);
 useEffect(()=>()=>{++selectionSeq.current;stopBatch.current=true;},[]);
 useEffect(()=>{
  const warn=(e:BeforeUnloadEvent)=>{if(batchRunning.current||batchRequests.current.some(item=>item.state==="unknown")){e.preventDefault();e.returnValue="";}};
  window.addEventListener("beforeunload",warn);return()=>window.removeEventListener("beforeunload",warn);
 },[]);
 const resetSelection=()=>{++selectionSeq.current;setChecked({});setSelecting(false);setOffset(0);};
 const toggle=(row:Row,on:boolean)=>setChecked(old=>{const next={...old};if(on)next[row.ref]=row;else delete next[row.ref];return next;});
 const pageRows=data.candidates.filter(row=>eligible(row,data.records));
 const selectedRows=Object.values(checked).filter(row=>eligible(row,data.records));
 const selectAll=async()=>{
  const seq=++selectionSeq.current;setSelecting(true);setError("");
  try{
   const rows:Record<string,Row>={};let total=0;
   for(let start=0;start===0||start<total;start+=25){
    const value=await request<Listing>(listURL(start));if(seq!==selectionSeq.current)return;
    if(start===0)total=value.total;
    for(const row of value.candidates)if(eligible(row,value.records))rows[row.ref]=row;
    if(!value.candidates.length)break;
   }
   if(seq===selectionSeq.current)setChecked(rows);
  }catch(e){if(seq===selectionSeq.current)setError((e as Error).message);}finally{if(seq===selectionSeq.current)setSelecting(false);}
 };
 const beginBatch=()=>{
  batchRequests.current=selectedRows.map(row=>({row,request:{ref:row.ref,request_id:crypto.randomUUID(),options:{}},state:"ready"}));
  setBatch([...batchRequests.current]);setBatchMaterials(true);setBatchCredentials(true);setBatchOpen(true);
 };
 const runBatch=async()=>{
  if(batchRunning.current)return;
  batchRunning.current=true;stopBatch.current=false;setBatchBusy(true);
  try{
   for(const item of batchRequests.current){
    if(stopBatch.current)break;
    if(item.state==="submitted")continue;
    // Freeze each payload before sending; unknown responses retain it verbatim.
    if(item.state==="ready")item.request.options={include_materials:batchMaterials,include_credentials:batchCredentials,fields:{}};
    item.state="sending";item.error="";setBatch([...batchRequests.current]);
    try{
     const receipt=await send<Row>("/api/sharing/consent",item.request);
     if(receipt.status==="rejected"){
      item.state="failed";item.error=receipt.error||"未接收，可在移交记录修改后重提";
     }else{item.state="submitted";toggle(item.row,false);}
    }catch(e){item.state=e instanceof ApiError&&e.status>=400&&e.status<500&&e.status!==408?"failed":"unknown";item.error=(e as Error).message;}
    setBatch([...batchRequests.current]);
   }
  }finally{batchRunning.current=false;setBatchBusy(false);await load();}
 };
 const inspect=async(row:Row)=>{
  const seq=++previewSeq.current;setSelected(row);setPreview(null);setFields({});setSms("");setJson("");setMaterials(true);setCredentials(true);setUncertain(false);setFormError("");original.current=null;
  try{const v=await send<Preview>("/api/sharing/preview",{ref:row.ref,options:{}});if(seq===previewSeq.current)setPreview(v);}catch(e){if(seq===previewSeq.current)setFormError((e as Error).message);}
 };
 const close=()=>{if(!saving&&!uncertain){++previewSeq.current;setSelected(null);setFields({});setJson("");original.current=null;}};
 const submit=async()=>{
  if(!selected||sending.current)return;
  sending.current=true;setSaving(true);setFormError("");
  try{
   if(!original.current){
    const values:Record<string,unknown>=Object.fromEntries(Object.entries(fields).filter(([,v])=>v!==""));
    if(fields.registered_platforms)values.registered_platforms=fields.registered_platforms.split(/[,，\s]+/).filter(Boolean);
    if(sms!=="")values.sms_enabled=sms==="yes";
    if(json.trim()){let value:unknown;try{value=JSON.parse(json);}catch{throw new Error("JSON 格式无效");}values.credential_json=value;}
    original.current={ref:selected.ref,request_id:crypto.randomUUID(),options:{include_materials:materials,include_credentials:credentials,fields:values}};
   }
   await send("/api/sharing/consent",original.current);original.current=null;setSelected(null);setFields({});setJson("");setUncertain(false);await load();
  }catch(e){const known=e instanceof ApiError&&e.status>=400&&e.status<500&&e.status!==408;if(known){original.current=null;setUncertain(false);}else if(original.current)setUncertain(true);setFormError((e as Error).message);}
  finally{sending.current=false;setSaving(false);}
 };
 const retry=async(row:Row)=>{setError("");try{await send("/api/sharing/sync",{ref:row.ref});}catch(e){setError((e as Error).message);}await load();};
 const historyRows=data.records.filter(row=>(type==="all"||(type==="mailbox")===(category(row)==="邮箱"))&&row.ref.toLowerCase().includes(query.toLowerCase()));
 const receivedRows=data.received.filter(row=>row.account_id.toLowerCase().includes(query.toLowerCase()));
 return <div className="asset-sharing min-w-0 space-y-3">
  <header className="flex items-center justify-between gap-3"><h1 className="text-xl font-semibold">{titles[view]}</h1><button className="btn-ghost" disabled={busy} onClick={()=>{setError("");void load();}}>刷新</button></header>
  {error&&<p role="alert" className="rounded-lg border border-destructive/30 p-3 text-sm text-destructive">{error}</p>}
  <section className="min-w-0" aria-label={titles[view]}>
   <form className="mb-4 flex flex-wrap items-center gap-3" onSubmit={e=>{e.preventDefault();resetSelection();setQuery(search);}}>
    {view!=="received"&&<select aria-label="资料分类" className="field w-auto" value={type} disabled={batchBusy} onChange={e=>{resetSelection();setType(e.target.value);}}><option value="all">全部分类</option><option value="account">账号</option><option value="mailbox">邮箱</option></select>}
    <input className="field min-w-0 flex-1 sm:max-w-80" aria-label="搜索资料" placeholder={view==="received"?"搜索资产编号":"搜索账号 / 邮箱"} value={search} disabled={batchBusy} onChange={e=>setSearch(e.target.value)}/><button className="btn-ghost" disabled={busy||batchBusy}>搜索</button>
   </form>
   {view==="current"&&<>
    <div className="mb-3 grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3 sm:flex sm:flex-wrap">
     <span className="text-sm text-muted-foreground">共 {data.total} 项 · 已选 {selectedRows.length} 项</span>
     <button className="btn-primary sm:order-3" disabled={!selectedRows.length||selecting||batchBusy||batch.some(item=>item.state==="unknown")} onClick={beginBatch}>批量移交{selectedRows.length?` (${selectedRows.length})`:""}</button>
     <div className="col-span-2 flex flex-wrap items-center gap-3 sm:order-2"><button className="btn-ghost" disabled={busy||selecting||batchBusy||!data.total} onClick={()=>void selectAll()}>{selecting?"选择中…":"选择筛选下全部"}</button><button className="btn-ghost" disabled={!selectedRows.length||batchBusy} onClick={()=>setChecked({})}>清除选择</button></div>
     {batch.length>0&&<button className="btn-ghost col-span-2 justify-self-start sm:order-4" onClick={()=>setBatchOpen(true)}>查看批量结果</button>}
    </div>
    <div className="overflow-x-auto rounded-lg border border-border"><table className="tbl min-w-[660px]" aria-label="节点现有资料"><thead><tr><th className="w-12"><input type="checkbox" aria-label="全选本页" disabled={!pageRows.length||selecting||batchBusy} checked={!!pageRows.length&&pageRows.every(row=>!!checked[row.ref])} ref={element=>{if(element)element.indeterminate=pageRows.some(row=>!!checked[row.ref])&&!pageRows.every(row=>!!checked[row.ref]);}} onChange={e=>{const on=e.target.checked;setChecked(old=>{const next={...old};for(const row of pageRows){if(on)next[row.ref]=row;else delete next[row.ref];}return next;});}}/></th><th>账号 / 邮箱</th><th>分类</th><th>平台</th><th>状态</th><th>操作</th></tr></thead><tbody>{data.candidates.map(row=><tr key={row.ref}><td><input type="checkbox" aria-label={`选择 ${row.email||row.ref}`} checked={!!checked[row.ref]&&eligible(row,data.records)} disabled={!eligible(row,data.records)||selecting||batchBusy} onChange={e=>toggle(row,e.target.checked)}/></td><td className="max-w-80 break-all">{row.email||row.ref}</td><td>{category(row)}</td><td>{row.platform||"—"}</td><td>{row.status||"—"}</td><td><button className="btn-ghost whitespace-nowrap" disabled={!eligible(row,data.records)||batchBusy} onClick={()=>void inspect(row)}>{eligible(row,data.records)?"移交":"已提交"}</button></td></tr>)}{!data.candidates.length&&<tr><td colSpan={6} className="h-24 text-center text-muted-foreground">{busy?"读取中…":"没有符合条件的资料"}</td></tr>}</tbody></table></div>
    <div className="mt-4 flex items-center justify-end gap-3"><span className="text-sm text-muted-foreground">第 {Math.floor(offset/25)+1} 页</span><button className="btn-ghost" disabled={!offset||busy||batchBusy} onClick={()=>setOffset(Math.max(0,offset-25))}>上一页</button><button className="btn-ghost" disabled={offset+25>=data.total||busy||batchBusy} onClick={()=>setOffset(offset+25)}>下一页</button></div>
   </>}
   {view==="history"&&<div className="overflow-x-auto rounded-lg border border-border"><table className="tbl min-w-[640px]" aria-label="移交记录"><thead><tr><th>资料</th><th>分类</th><th>状态</th><th>接收时间</th><th>操作</th></tr></thead><tbody>{historyRows.map(row=><tr key={row.ref}><td className="max-w-80 break-all">{row.ref}</td><td>{category(row)}</td><td>{stateNames[row.status||""]||row.status}{row.error&&<div className="mt-1 text-xs text-destructive">{row.error}</div>}</td><td>{stamp(row.last_sync)}</td><td>{!["transferred","rejected"].includes(row.status||"")&&<button className="btn-ghost whitespace-nowrap" onClick={()=>void retry(row)}>重试原移交</button>}{row.status==="rejected"&&<button className="btn-ghost whitespace-nowrap" onClick={()=>void inspect(row)}>修改后重提</button>}</td></tr>)}{!historyRows.length&&<tr><td colSpan={5} className="h-24 text-center text-muted-foreground">暂无移交记录</td></tr>}</tbody></table></div>}
   {view==="received"&&<div className="overflow-x-auto rounded-lg border border-border"><table className="tbl min-w-[560px]" aria-label="收到的资料"><thead><tr><th>资产</th><th>版本</th><th>收到时间</th></tr></thead><tbody>{receivedRows.map(row=><tr key={row.task_id}><td className="break-all">{row.account_id}</td><td>v{row.revision}</td><td>{stamp(row.received_at)}</td></tr>)}{!receivedRows.length&&<tr><td colSpan={3} className="h-24 text-center text-muted-foreground">暂无收到的资料</td></tr>}</tbody></table></div>}
  </section>
  <Drawer open={batchOpen} title="批量移交" variant="dialog" width={720} onClose={()=>{if(!batchBusy)setBatchOpen(false);}} footer={<><button className="btn-ghost" disabled={batchBusy} onClick={()=>setBatchOpen(false)}>关闭</button>{batchBusy?<button className="btn-ghost" onClick={()=>{stopBatch.current=true;}}>停止后续提交</button>:batch.some(item=>item.state!=="submitted")&&<button className="btn-primary" onClick={()=>void runBatch()}>{batch.every(item=>item.state==="ready")?"确认批量移交":"重试未完成项"}</button>}</>}>
   <p className="mb-4 text-sm">共 {batch.length} 项 · 已提交 {batch.filter(item=>item.state==="submitted").length} 项 · 未确认 {batch.filter(item=>item.state==="unknown").length} 项 · 失败 {batch.filter(item=>item.state==="failed").length} 项</p>
   <fieldset disabled={batchBusy||batch.some(item=>item.state!=="ready")} className="mb-5 flex flex-wrap gap-5"><label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={batchMaterials} onChange={e=>setBatchMaterials(e.target.checked)}/>账号 / 邮箱资料</label><label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={batchCredentials} onChange={e=>setBatchCredentials(e.target.checked)}/>凭据 JSON</label></fieldset>
   <div className="overflow-x-auto"><table className="tbl min-w-[420px]" aria-label="批量移交结果"><thead><tr><th>资料</th><th>结果</th></tr></thead><tbody>{batch.map(item=><tr key={item.row.ref}><td className="max-w-80 break-all">{item.row.email||item.row.ref}</td><td>{{ready:"待提交",sending:"提交中",submitted:"已提交",failed:"未提交",unknown:"提交未确认"}[item.state]}{item.error&&<p className="mt-1 text-xs text-destructive">{item.error}</p>}</td></tr>)}</tbody></table></div>
  </Drawer>
  <Drawer open={!!selected} title="移交资料" variant="dialog" width={680} onClose={close} footer={<><button className="btn-ghost" disabled={saving||uncertain} onClick={close}>关闭</button><button className="btn-primary" disabled={saving} onClick={()=>void submit()}>{saving?"提交中":uncertain?"重试原请求":"确认移交"}</button></>}>
   <p className="mb-5 break-all text-sm font-medium">{preview?.asset.email||selected?.ref}</p>
   <fieldset disabled={saving||uncertain} className="space-y-5">
    <div className="flex flex-wrap gap-5"><label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={materials} onChange={e=>setMaterials(e.target.checked)}/>账号 / 邮箱资料</label><label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={credentials} onChange={e=>setCredentials(e.target.checked)}/>凭据 JSON</label></div>
    {materials&&<div className="grid gap-4 sm:grid-cols-2">{fieldsList.map(([key,label])=><label key={key} className="block space-y-2 text-sm"><span>{label}</span><input aria-label={label} className="field" type={/password|2fa/.test(key)?"password":"text"} autoComplete="off" value={fields[key]||""} placeholder={preview?.present.includes(key)?"已保存，留空保留":"可选"} onChange={e=>setFields({...fields,[key]:e.target.value})}/></label>)}<label className="block space-y-2 text-sm"><span>是否接码</span><select aria-label="是否接码" className="field" value={sms} onChange={e=>setSms(e.target.value)}><option value="">未填写</option><option value="yes">是</option><option value="no">否</option></select></label></div>}
    {credentials&&<label className="block space-y-2 text-sm"><span>凭据 JSON</span><textarea aria-label="凭据 JSON" className="field min-h-36 font-mono text-xs" autoComplete="off" value={json} onChange={e=>setJson(e.target.value)} placeholder="可选，留空使用已保存的凭据"/></label>}
   </fieldset>
   {formError&&<p role="alert" className="mt-4 text-sm text-destructive">{formError}</p>}
  </Drawer>
 </div>;
}
