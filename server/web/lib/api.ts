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

export type LogEvent = { ts: string; stream: string; line: string };

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

export type AccountRow = {
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

async function request<T>(path: string, init?: RequestInit, options?: { noRedirect?: boolean }): Promise<T> {
  const response = await fetch(path, {
    ...init,
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
      const next = encodeURIComponent(window.location.pathname);
      window.location.href = `/login/?next=${next}`;
    }
    throw new ApiError(body.error || `HTTP ${response.status}`, response.status, body.detail);
  }
  return payload as T;
}

export const api = {
  session: () =>
    request<{ ok: boolean; user: string; actor: string; anon: boolean }>("/api/session", undefined, {
      noRedirect: true,
    }),
  login: (username: string, password: string) =>
    request<{ ok: boolean }>("/api/login", { method: "POST", body: JSON.stringify({ username, password }) }),
  logout: () => request<{ ok: boolean }>("/api/logout", { method: "POST" }),
  system: () => request<SystemInfo>("/api/system"),
  memory: () => request<{ current_mb: number; max_mb: number; used_pct: number }>("/api/memory"),
  scripts: () => request<{ scripts: Script[]; count: number }>("/api/scripts"),
  tasks: (limit = 100) => request<{ tasks: Task[]; count: number }>(`/api/tasks?limit=${limit}`),
  task: (id: string) => request<Task>(`/api/tasks/${id}`),
  createTask: (script: string, args: Record<string, unknown>) =>
    request<{ id: string; status: string; title: string }>("/api/tasks", {
      method: "POST",
      body: JSON.stringify({ script, args }),
    }),
  stopTask: (id: string) => request<{ ok: boolean }>(`/api/tasks/${id}/stop`, { method: "POST" }),
  stopAll: () => request<{ ok: boolean; stopped: string[] }>("/api/tasks/stop-all", { method: "POST" }),
  logs: (id: string, tail = 500) => request<{ events: LogEvent[] }>(`/api/tasks/${id}/logs?tail=${tail}`),
  assetSummary: () =>
    request<{ assets: { kind: string; total: number; claimed: number; available: number }[] }>("/api/assets"),
  assetList: (kind: string, limit = 200) =>
    request<{ kind: string; items: AssetItem[]; count: number }>(`/api/assets/${kind}?limit=${limit}`),
  claim: (kind: string) => request<{ ok: boolean; item: AssetItem }>(`/api/assets/${kind}/claim`, { method: "POST" }),
  resetClaims: (kind: string) =>
    request<{ ok: boolean; removed: number }>(`/api/assets/claims/reset?kind=${encodeURIComponent(kind)}`, {
      method: "POST",
    }),
  config: () =>
    request<{ path: string; groups: Record<string, { key: string; value: string; secret: boolean; line: number }[]> }>(
      "/api/config",
    ),
  saveConfig: (updates: Record<string, string>) =>
    request<{ ok: boolean; changed: string[] }>("/api/config", { method: "PUT", body: JSON.stringify({ updates }) }),
  configRaw: () => request<{ path: string; content: string }>("/api/config/raw"),
  saveRaw: (content: string) => request<{ ok: boolean }>("/api/config/raw", { method: "PUT", body: JSON.stringify({ content }) }),
  outlookAuthorize: (accounts: string, concurrency = 3) =>
    request<{ ok: boolean; task_id: string }>("/api/outlook/authorize", {
      method: "POST",
      body: JSON.stringify({ accounts, concurrency }),
    }),
  accounts: (q: { kind?: string; platform?: string; status?: string; limit?: number } = {}) => {
    const params = new URLSearchParams();
    if (q.kind) params.set("kind", q.kind);
    if (q.platform) params.set("platform", q.platform);
    if (q.status) params.set("status", q.status);
    params.set("limit", String(q.limit ?? 200));
    return request<{ accounts: AccountRow[]; count: number }>(`/api/accounts?${params.toString()}`);
  },
  syncAccounts: () => request<{ ok: boolean; synced: number }>("/api/accounts/sync", { method: "POST" }),
  accountLiveness: (ref: string, platform: string, refreshScan = false) =>
    request<{ ok: boolean; task_id: string }>("/api/accounts/liveness", {
      method: "POST",
      body: JSON.stringify({ ref, platform, refresh_scan: refreshScan }),
    }),
  accountRefresh: (ref: string, platform: string, allowRelogin = false) =>
    request<{ ok: boolean; task_id: string }>("/api/accounts/refresh", {
      method: "POST",
      body: JSON.stringify({ ref, platform, allow_relogin: allowRelogin }),
    }),
  audit: (limit = 200) => request<{ audit: { id: number; ts: string; actor: string; action: string; detail: string; ip: string }[] }>(`/api/audit?limit=${limit}`),
  proxyCheck: (proxy: string, url: string) =>
    request<{ ok: boolean; status?: number; ms?: number; title?: string; error?: string; proxy?: string }>(
      `/api/proxy/check?proxy=${encodeURIComponent(proxy)}&url=${encodeURIComponent(url)}`,
    ),
};
