/** 任务目录：把 schema 里的任务按业务分组（前端集中定义，不改脚本 schema）。 */

export type TaskGroup = {
  slug: string;
  title: string;
  desc: string;
  /** 该组包含的 script id */
  ids: string[];
};

export const TASK_GROUPS: TaskGroup[] = [
  {
    slug: "register",
    title: "注册",
    desc: "从邮箱到平台账号的产出流程；单平台与端到端都在这里。",
    ids: [
      "run_full_flow",
      "register_three_platforms",
      "register_claude",
      "register_chatgpt",
      "register_grok",
      "register_kiro",
      "register_github",
      "outlook_reg_loop",
    ],
  },
  {
    slug: "authorize",
    title: "授权",
    desc: "把已注册账号换成可长期使用的凭据（OAuth / Graph / 工作区）。",
    ids: ["oauth_codex", "unlock_outlook", "plus_codex_import"],
  },
  {
    slug: "data",
    title: "数据",
    desc: "导出、转换与上传：把账号变成下游可消费的文件或接口调用。",
    ids: ["export_accounts", "export_chatgpt2api", "export_kiro_credentials", "upload_tokens"],
  },
  {
    slug: "helper",
    title: "辅助",
    desc: "并发取码、环境准备等支撑性任务。",
    ids: ["mailbox_broker", "download_clash_verge", "download_bitbrowser"],
  },
];

const INDEX = new Map<string, TaskGroup>();
for (const group of TASK_GROUPS) {
  for (const id of group.ids) INDEX.set(id, group);
}

/** 按 script id 找组；未知 id 归入「其他」。 */
export function groupOf(scriptId: string): TaskGroup {
  return (
    INDEX.get(scriptId) || {
      slug: "other",
      title: "其他",
      desc: "未归类的任务。",
      ids: [],
    }
  );
}

export function groupBySlug(slug: string): TaskGroup | undefined {
  return TASK_GROUPS.find((item) => item.slug === slug);
}

/** 全部组（含 other，用于静态导出）。 */
export function allGroupSlugs(): string[] {
  return [...TASK_GROUPS.map((item) => item.slug), "other"];
}
