export type ScriptArg = {
  flag: string;
  type: string;
  default?: unknown;
  help?: string;
  choices?: string[];
  labels?: Record<string, string>;
  positional?: boolean;
};

export type Script = {
  id: string;
  title: string;
  file: string;
  desc?: string;
  platform?: string;
  platforms?: string[];
  group?: string;
  category?: string;
  warning?: string;
  args?: ScriptArg[];
};

export type Task = {
  id: string;
  script: string;
  title: string;
  args_json: string;
  status: string;
  pid: number;
  created_at: string;
  started_at: string;
  finished_at: string;
  exit_code: number;
  error: string;
  actor: string;
};

export type LogEvent = { id: number; task_id: string; ts: string; stream: string; line: string };

export type TaskPage = { tasks: Task[]; count: number; total: number; stats: Record<string, number>; limit: number; offset: number };
export type CredentialCheckItem = { ordinal: number; ref: string; identity: string; status: string; message: string; checked_at: string; child_task_id: string; mode: "at" | "report"; can_refresh: boolean };
export type CredentialCheckCounts = { total: number; checked: number; ok: number; needs_refresh: number; unknown: number; abnormal: number };
export type CredentialCheckDetail = { task: Task; items: CredentialCheckItem[] };
export type CredentialCheckHistory = { page: TaskPage; counts: Record<string, CredentialCheckCounts> };
export type TaskFilter = { status?: string; q?: string; scripts?: string[]; offset?: number };
export type AccountSummary = { platform: string; total: number; available: number; abnormal: number; unknown: number; used: number; leased: number; source_count: number };
export type ConfigEntry = { key: string; value: string; secret: boolean; line: number; label?: string; help?: string; type?: string; choices?: string[]; default?: unknown; apply?: string; readonly?: boolean; advanced?: boolean; generate?: boolean; area?: string; empty_default?: boolean; effective_value?: string; effective_source?: string; effective_set?: boolean; overridden?: boolean };

export type SystemInfo = {
  version: string;
  uptime_sec: number;
  data_dir: string;
  env_file: string;
  addr: string;
  python: string;
  max_parallel: number;
  slots_used: number;
  slots_total: number;
  cpu_cores: number;
  cpu_quota: number;
  cpu_pct: number;
  running: number;
  queued: number;
  guarded: boolean;
  guard_pct: number;
  memory: { current_mb: number; max_mb: number; used_pct: number };
  disk_total_gb: number;
  disk_free_gb: number;
  task_stats: Record<string, number>;
  server_time: string;
};

export type NodeCapabilities = {
  ok: boolean;
  node_id: string;
  config: string;
  schema: string;
  business_key_fp: string;
  receipt_key_fp: string;
  work_key_fp: string;
  actions: { supported: string[]; unsupported: string[]; notes: Record<string, string> };
  detail: Record<string, unknown>;
};

export type NodeReady = {
  ready: boolean;
  execution_ready: boolean;
  action_readiness?: Record<string, {ready: boolean; error_code?: string; processing_owner?: string}>;
  runtime: { supported_actions: string[]; protocol_capabilities: string[] };
};

export type AccountRow = {
	created_at?: string;
	imported_at?: string;
	import_batch?: string;
	has_credentials?: boolean;

  credential_updated_at?: string;
  credential_ref?: string;
	 library_status?: string;
  source_count?: number;
  version?: number;
  oauth_status?: string;
  join_status?: string;
  liveness_status?: string;
  liveness_checked_at?: string;
  ref: string;
  platform: string;
  kind: string;
  identity: string;
  secret_ref: string;
  status: string;
  health_checked_at: string;
  health_message: string;
  claimed: boolean;
  claimed_at: string;
  refresh_state: string;
  refresh_at: string;
  note: string;
  updated_at: string;
  password_state?: string;
  password_changed_at?: string;
  password_message?: string;
};

export type AssetItem = {
  ref: string;
  kind: string;
  platform: string;
  value: string;
  source: string;
  claimed: boolean;
};

export class ApiError extends Error {
  status: number;
  detail?: string;
  constructor(message: string, status: number, detail?: string) {
    super(message);
    this.status = status;
    this.detail = detail;
  }
}

export async function request<T>(path: string, init?: RequestInit, options?: { noRedirect?: boolean; timeoutMs?: number }): Promise<T> {
  const controller = new AbortController();
  let timedOut = false;
  const abort = () => controller.abort();
  init?.signal?.addEventListener("abort", abort, { once: true });
  if (init?.signal?.aborted) abort();
  const timer = setTimeout(() => { timedOut = true; abort(); }, options?.timeoutMs ?? 60000);
  try {
  const response = await fetch(path, {
    ...init,
    signal: controller.signal,
    headers: {
      "Content-Type": "application/json",
      ...(init?.headers || {}),
    },
    credentials: "same-origin",
    cache: "no-store",
  });
  const text = await response.text();
  let payload: unknown = null;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = { raw: text };
    }
  }
  if (!response.ok) {
    const body = (payload || {}) as { error?: string; detail?: string };
    // 只有“页面级的会话失效”才跳登录；登录页自身与登录接口不跳，避免无限重定向
    if (
      response.status === 401 &&
      !options?.noRedirect &&
      typeof window !== "undefined" &&
      !path.startsWith("/api/login") &&
      !path.startsWith("/api/session") &&
      !window.location.pathname.startsWith("/login")
    ) {
      const next = encodeURIComponent(window.location.pathname + window.location.search);
      window.location.href = `/login/?next=${next}`;
    }
    throw new ApiError(body.error || `HTTP ${response.status}`, response.status, body.detail);
  }
  return payload as T;
  } catch (error) {
    if (timedOut) throw new ApiError("请求超时，请核对原结果后重试", 408);
    throw error;
  } finally {
    clearTimeout(timer);
    init?.signal?.removeEventListener("abort", abort);
  }
}

export const api = {
  createCredentialCheck: (body: {request_id: string; platform: string; refs: string[]; parent_id?: string}) =>
    request<{task_id: string; status: string}>("/api/accounts/checks", {method:"POST", body:JSON.stringify(body)}, {timeoutMs:20000}),
  credentialChecks: (platform: string, action: string, status: string, offset: number, signal?: AbortSignal) =>
    request<CredentialCheckHistory>(`/api/accounts/checks?${new URLSearchParams({platform,action,status,offset:String(offset)})}`, {signal}, {timeoutMs:20000}),
  credentialCheck: (id: string, signal?: AbortSignal) =>
    request<CredentialCheckDetail>(`/api/accounts/checks/${encodeURIComponent(id)}`, {signal}, {timeoutMs:20000}),
  icMailHealth: async (signal?: AbortSignal) => {
    const result = await request<{ok: boolean; data: {stats: {indexed: number; sources: number}}; error?: string}>("/api/apps/icmail/health", {signal});
    if (!result.ok) throw new Error(result.error || "IC 索引读取失败");
    return result.data;
  },
  icMailSources: async (signal?: AbortSignal) => {
    const result = await request<{ok: boolean; data: {sources: {id: number; name: string; host: string; port: number; username: string; folder: string; enabled: boolean; lastUID: number}[]}; error?: string}>("/api/apps/icmail/sources", {signal});
    if (!result.ok) throw new Error(result.error || "IC 源读取失败");
    return result.data;
  },
  productionControl: (id: string, action: "continue" | "stop") =>
    request<{ ok: boolean }>(`/api/tasks/${encodeURIComponent(id)}/production/${action}`, { method: "POST" }),
  session: () =>
    request<{ ok: boolean; user: string; actor: string; anon: boolean }>("/api/session", undefined, {
      noRedirect: true,
    }),
  login: (username: string, password: string) =>
    request<{ ok: boolean }>("/api/login", { method: "POST", body: JSON.stringify({ username, password }) }),
  logout: () => request<{ ok: boolean }>("/api/logout", { method: "POST" }),
  system: (signal?: AbortSignal) => request<SystemInfo>("/api/system", { signal }),
  nodeReady: (signal?: AbortSignal) => request<NodeReady>("/ready", {signal}),
  nodeCapabilities: (signal?: AbortSignal) => request<NodeCapabilities>("/node/capabilities", {signal}),
  memory: () => request<{ current_mb: number; max_mb: number; used_pct: number }>("/api/memory"),
  scripts: (signal?: AbortSignal) => request<{ scripts: Script[]; count: number }>("/api/scripts", { signal }),
  tasks: (limit = 100, filter: TaskFilter = {}, signal?: AbortSignal) => {
    const params = new URLSearchParams({ limit: String(limit), offset: String(filter.offset || 0) });
    if (filter.status) params.set("status", filter.status);
    if (filter.q) params.set("q", filter.q);
    for (const script of filter.scripts || []) params.append("script", script);
    return request<TaskPage>(`/api/tasks?${params}`, { signal }, { timeoutMs: 20000 });
  },
  task: (id: string, signal?: AbortSignal) => request<Task>(`/api/tasks/${encodeURIComponent(id)}`, { signal }, { timeoutMs: 20000 }),
  createTask: (script: string, args: Record<string, unknown>, request_id?: string) =>
    request<{ id: string; status: string; title: string }>("/api/tasks", {
      method: "POST",
      body: JSON.stringify({ script, args, request_id }),
    }, { timeoutMs: 20000 }),
  stopTask: (id: string) => request<{ ok: boolean }>(`/api/tasks/${encodeURIComponent(id)}/stop`, { method: "POST" }),
  stopAll: () => request<{ ok: boolean; stopped: string[] }>("/api/tasks/stop-all", { method: "POST" }),
  logs: (id: string, tail = 500) => request<{ events: LogEvent[] }>(`/api/tasks/${encodeURIComponent(id)}/logs?tail=${tail}`),
  assetSummary: () =>
    request<{ assets: { kind: string; total: number; claimed: number; available: number }[] }>("/api/assets"),
  assetList: (kind: string, limit = 200) =>
    request<{ kind: string; items: AssetItem[]; count: number }>(`/api/assets/${kind}?limit=${limit}`),
  claim: (kind: string) => request<{ ok: boolean; item: AssetItem }>(`/api/assets/${kind}/claim`, { method: "POST" }),
  resetClaims: (kind: string) =>
    request<{ ok: boolean; removed: number }>(`/api/assets/claims/reset?kind=${encodeURIComponent(kind)}`, {
      method: "POST",
    }),
  config: (signal?: AbortSignal) =>
    request<{ path: string; groups: Record<string, ConfigEntry[]>; schema_error?: string }>(
      "/api/config", { signal },
    ),
  saveConfig: (updates: Record<string, string>) =>
    request<{ ok: boolean; changed: string[] }>("/api/config", { method: "PUT", body: JSON.stringify({ updates }) }),
  configRaw: () => request<{ path: string; content: string }>("/api/config/raw"),
  saveRaw: (content: string) => request<{ ok: boolean }>("/api/config/raw", { method: "PUT", body: JSON.stringify({ content }) }),
  claudeHTTPRegister: (body: {
    email: string;
    refresh_token?: string;
    client_id?: string;
    proxy?: string;
    magic_link?: string;
    mailbox?: string;
  }) =>
    request<{ ok: boolean; email: string; session_key_len: number; saved_to: string }>(
      "/api/registry/claude-http",
      { method: "POST", body: JSON.stringify(body) },
    ),
  outlookAuthorize: (accounts: string, concurrency = 3) =>
    request<{ ok: boolean; task_id: string }>("/api/outlook/authorize", {
      method: "POST",
      body: JSON.stringify({ accounts, concurrency }),
    }),
  accounts: (q: { kind?: string; platform?: string; status?: string; q?: string; batch?: string; library_status?: string; limit?: number; offset?: number } = {}, signal?: AbortSignal) => {
    const params = new URLSearchParams();
    if (q.kind) params.set("kind", q.kind);
    if (q.platform) params.set("platform", q.platform);
    if (q.status) params.set("status", q.status);
    if (q.q) params.set("q", q.q);
    if (q.batch) params.set("batch", q.batch);
    if (q.library_status) params.set("library_status", q.library_status);
    params.set("limit", String(q.limit ?? 200));
    params.set("offset", String(q.offset ?? 0));
    return request<{ accounts: AccountRow[]; count: number; total: number }>(`/api/accounts?${params.toString()}`, { signal });
  },
  convertAccounts: (body: { refs: string[]; platform?: string; request_id?: string }) =>
    request<{ ok: boolean; queued: { ref: string; task_id: string; email: string }[]; skipped: { ref: string; reason: string }[] }>(
      "/api/accounts/convert",
      { method: "POST", body: JSON.stringify(body) },
    ),
  changePasswords: (body: { refs: string[]; new_password: string; platform?: string }) =>
    request<{ ok: boolean; queued: { ref: string; task_id: string }[]; skipped: { ref: string; reason: string }[] }>(
      "/api/accounts/change-password",
      { method: "POST", body: JSON.stringify(body) },
    ),
  importAccounts: (body: {
    accounts: string;
    platform: string;
    auto_change_password?: boolean;
    new_password?: string;
  }) =>
    request<{ ok: boolean; batch: string; imported: number; tasks: string[] }>("/api/accounts/import", {
      method: "POST",
      body: JSON.stringify(body),
    }),
  assetsTree: () =>
    request<{
      root: string;
      total: number;
      groups: { platform: string; kind: string; count: number; items: { ref: string; email: string; kind: string; source: string; created_at?: string; result_sha256: string }[] }[];
    }>("/api/assets/tree"),
  assetResults: (ref: string, query = "") => request<{items: {ref: string; email: string; kind: string; source: string; created_at?: string; credential_updated_at?: string; result_sha256: string; is_current?: boolean}[]}>(`/api/assets/results?ref=${encodeURIComponent(ref)}&query=${encodeURIComponent(query)}`),
  accountDetail: (ref: string, signal?: AbortSignal) =>
    request<{ account: AccountRow }>(`/api/accounts/detail?ref=${encodeURIComponent(ref)}`, {signal}),
  accountSummary: (platform = "", signal?: AbortSignal) => request<{ platforms: AccountSummary[] }>(`/api/accounts/summary?platform=${encodeURIComponent(platform)}`, { signal }),
  accountSources: (ref: string, signal?: AbortSignal) => request<{accounts: AccountRow[]; total: number}>(`/api/accounts/sources?ref=${encodeURIComponent(ref)}`, {signal}),
  syncAccounts: () => request<{ ok: boolean; synced: number }>("/api/accounts/sync", { method: "POST" }),
  accountLiveness: (ref: string, platform: string, refreshScan = false) =>
    request<{ ok: boolean; task_id?: string; status?: string; message?: string }>("/api/accounts/liveness", {
      method: "POST",
      body: JSON.stringify({ ref, platform, refresh_scan: refreshScan }),
    }),
  accountRefresh: (ref: string, platform: string, allowRelogin = false, request_id?: string) =>
    request<{ ok: boolean; task_id: string }>("/api/accounts/refresh", {
      method: "POST",
      body: JSON.stringify({ ref, platform, allow_relogin: allowRelogin, request_id }),
    }),
  accountRefreshDetail: (id: string, signal?: AbortSignal) => request<RefreshDetail>(`/api/accounts/refresh/${encodeURIComponent(id)}`,{signal}),
  reconcileRefresh: (id: string) => request<RefreshDetail>(`/api/accounts/refresh/${encodeURIComponent(id)}/reconcile`,{method:"POST"}),
  resolveRefresh: (id: string, reason: string) => request<RefreshDetail>(`/api/accounts/refresh/${encodeURIComponent(id)}/resolve`,{method:"POST",body:JSON.stringify({reason})}),
  audit: (limit = 200) => request<{ audit: { id: number; ts: string; actor: string; action: string; detail: string; ip: string }[] }>(`/api/audit?limit=${limit}`),
  proxyCheck: (proxy: string, url: string) =>
    request<{ ok: boolean; status?: number; ms?: number; title?: string; error?: string; proxy?: string }>(
      `/api/proxy/check?proxy=${encodeURIComponent(proxy)}&url=${encodeURIComponent(url)}`,
    ),
};

export type RefreshDetail = {
  method: "refresh" | "authorize";
  progress: {stage: string; status: string; next_action?: string; attempt?: number} | null;
	 library_status: string;
	 verification_task_id: string;
	 verification_status: string;
  task: Task;
  evidence: {ref: string; source_sha: string; result_sha: string; resolution: string};
  can_reconcile: boolean;
  can_resolve: boolean;
  can_retry: boolean;
};
