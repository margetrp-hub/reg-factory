import { api, ApiError } from "@/lib/api";

export type CredentialUpdateMethod = "refresh" | "authorize";
export type LoginMode = "browser" | "protocol";
export const LOGIN_MODE_LABELS = {browser:"浏览器／邮箱授权", protocol:"密码＋2FA 协议授权"};
export const UPDATE_LABELS = {refresh:"RT 刷新", authorize:"登录重新授权"};
export type AccountActionReceipt = { task_id?: string; status?: string; message?: string; method?: CredentialUpdateMethod };

function requestID() {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 15) | 64; bytes[8] = (bytes[8] & 63) | 128;
  const hex = Array.from(bytes, value => value.toString(16).padStart(2, "0")).join("");
  return [hex.slice(0,8), hex.slice(8,12), hex.slice(12,16), hex.slice(16,20), hex.slice(20)].join("-");
}

type PendingUpdate = {id:string; method?:CredentialUpdateMethod; loginMode?:LoginMode};
const pendingRefresh = new Map<string, PendingUpdate>();

export function pendingCredentialUpdate(ref: string): PendingUpdate | null {
  const key = "sg-account-refresh:" + ref;
  try {
    const raw = sessionStorage.getItem(key);
    if (raw) {
      if (!raw.startsWith("{")) return {id:raw}; // Resume requests from the previous UI.
      const saved = JSON.parse(raw);
      if (typeof saved.id === "string" && ["refresh","authorize"].includes(saved.method)) return saved;
    }
  } catch { /* optional browser storage */ }
  return pendingRefresh.get(key) || null;
}
type PendingCheck = {request_id: string; platform: string; refs: string[]; parent_id?: string};
const pendingChecks = new Map<string, PendingCheck>();

export function pendingCredentialCheck(platform: string): PendingCheck | null {
  try {
    const saved = JSON.parse(sessionStorage.getItem("sg-credential-check:" + platform) || "null");
    if (saved?.platform === platform && typeof saved.request_id === "string" && Array.isArray(saved.refs)) return saved;
  } catch { /* optional browser storage */ }
  return pendingChecks.get(platform) || null;
}

export async function startCredentialCheck(platform: string, refs: string[], parent_id?: string) {
  const pending = pendingCredentialCheck(platform);
  if (pending && (JSON.stringify(pending.refs) !== JSON.stringify(refs) || pending.parent_id !== parent_id)) {
    throw new Error("还有未确认的原批次，请先点击“核对原批次”");
  }
  const request = pending || {request_id:requestID(), platform, refs, ...(parent_id ? {parent_id} : {})};
  const clear = () => { pendingChecks.delete(platform); try {sessionStorage.removeItem("sg-credential-check:" + platform);} catch { /* optional */ } };
  if (pending) {
    try {
      const detail = await api.credentialCheck("request-" + pending.request_id);
      clear(); return {task_id:detail.task.id,status:detail.task.status};
    } catch (error) { if (!(error instanceof ApiError) || error.status !== 404) throw error; }
  }
  pendingChecks.set(platform, request);
  try { sessionStorage.setItem("sg-credential-check:" + platform, JSON.stringify(request)); } catch { /* memory fallback */ }
  try { const result = await api.createCredentialCheck(request); clear(); return result; }
  catch (error) {
    if (error instanceof ApiError && [400,403,404,422,429].includes(error.status)) clear();
    throw error;
  }
}

// No credentials are persisted. A lost refresh response resolves the original
// task before submitting the same request ID again.
export async function accountAction(ref: string, platform: string, action: "check" | "refresh" | "authorize", loginMode?:LoginMode): Promise<AccountActionReceipt> {
  if (action === "check") return api.accountLiveness(ref, platform);
  const key = "sg-account-refresh:" + ref;
  const previous = pendingCredentialUpdate(ref);
  if (previous) {
    try {
      const task = await api.task("request-" + previous.id);
      pendingRefresh.delete(key);
      try { sessionStorage.removeItem(key); } catch { /* optional storage */ }
      const args = JSON.parse(task.args_json || "[]");
      return {task_id: task.id, status: task.status, message: "已找到原更新任务", method:Array.isArray(args) && args.includes("--allow-relogin") ? "authorize" : "refresh"};
    } catch (error) { if (!(error instanceof ApiError) || error.status !== 404) throw error; }
  }
  if (previous?.method && previous.method !== action) throw new Error(`原请求为${UPDATE_LABELS[previous.method]}，请先用原方式恢复提交`);
  const id = previous?.id || requestID();
  const pending = {id, method:action, loginMode:previous?.loginMode || loginMode};
  pendingRefresh.set(key, pending);
  try { sessionStorage.setItem(key, JSON.stringify(pending)); } catch { /* optional storage */ }
  try {
    const result = await api.accountRefresh(ref, platform, action === "authorize", id, pending.loginMode);
    pendingRefresh.delete(key);
    try { sessionStorage.removeItem(key); } catch { /* optional storage */ }
    return {...result,method:action};
  } catch (error) {
    if (error instanceof ApiError && [400,403,404,422,429].includes(error.status)) {
      pendingRefresh.delete(key);
      try { sessionStorage.removeItem(key); } catch { /* optional storage */ }
    }
    throw error;
  }
}
