export const CONFIG_CATEGORIES = [
  { id: "runtime", label: "运行与资源", description: "执行并发、资源保护和接口访问。" },
  { id: "proxy", label: "网络与代理", description: "统一管理注册、生产和凭据修复的出口，以及各平台的覆盖规则。" },
  { id: "browser", label: "浏览器", description: "选择要配置的浏览器；实际启用类型在通用设置中保存。" },
  { id: "mail", label: "邮箱与取码", description: "邮箱来源、供应商连接和收码等待统一放在这里。" },
  { id: "sms", label: "短信接码", description: "接码供应商、项目与国家，以及公共重试设置。" },
  { id: "captcha", label: "图形验证", description: "验证码平台、视觉识别和模型投票。" },
  { id: "platform", label: "平台规则", description: "各平台的注册、安全和授权规则。" },
  { id: "integration", label: "导入与对接", description: "下游导入目标、管理凭据和辅助控制台。" },
  { id: "custom", label: "其他配置", description: "保留尚未归入标准目录的自定义配置。" },
];

export function configCategory(area?: string): string {
  return CONFIG_CATEGORIES.some(category => category.id === area) ? area! : "custom";
}

export function configSectionLabel(group: string): string {
  return group.includes(" · ") ? group.split(" · ").slice(1).join(" · ") : group;
}
