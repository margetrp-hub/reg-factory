import { api, ApiError } from "@/lib/api";

export type AccountActionReceipt = { task_id?: string; status?: string; message?: string };

function requestID() {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 15) | 64; bytes[8] = (bytes[8] & 63) | 128;
  const hex = Array.from(bytes, value => value.toString(16).padStart(2, "0")).join("");
  return [hex.slice(0,8), hex.slice(8,12), hex.slice(12,16), hex.slice(16,20), hex.slice(20)].join("-");
}

const pendingRefresh = new Map<string, string>();
const pendingConversions = new Map<string, string>();

export async function convertAccountCredentials(refs: string[]) {
  const key = "sg-account-convert:" + JSON.stringify([...new Set(refs)].sort());
  let id = pendingConversions.get(key);
  try { id ||= sessionStorage.getItem(key) || undefined; } catch { /* optional storage */ }
  id ||= requestID();
  pendingConversions.set(key, id);
  try { sessionStorage.setItem(key,id); } catch { /* memory fallback */ }
  const result = await api.convertAccounts({refs,request_id:id});
  pendingConversions.delete(key);
  try { sessionStorage.removeItem(key); } catch { /* optional storage */ }
  return result;
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
export async function accountAction(ref: string, platform: string, action: "check" | "refresh" | "authorize"): Promise<AccountActionReceipt> {
  if (action === "check") return api.accountLiveness(ref, platform);
  const key = "sg-account-refresh:" + ref;
  let id = pendingRefresh.get(key);
  try { id ||= sessionStorage.getItem(key) || undefined; } catch { /* memory still protects this page */ }
  if (id) {
    try {
      const task = await api.task("request-" + id);
      pendingRefresh.delete(key);
      try { sessionStorage.removeItem(key); } catch { /* optional storage */ }
      return {task_id: task.id, status: task.status, message: "已找到原刷新任务"};
    } catch (error) { if (!(error instanceof ApiError) || error.status !== 404) throw error; }
  }
  id ||= requestID();
  pendingRefresh.set(key, id);
  try { sessionStorage.setItem(key, id); } catch { /* optional storage */ }
  try {
    const result = await api.accountRefresh(ref, platform, action === "authorize", id);
    pendingRefresh.delete(key);
    try { sessionStorage.removeItem(key); } catch { /* optional storage */ }
    return result;
  } catch (error) {
    if (error instanceof ApiError && [400,403,404,422,429].includes(error.status)) {
      pendingRefresh.delete(key);
      try { sessionStorage.removeItem(key); } catch { /* optional storage */ }
    }
    throw error;
  }
}
