// These are the node fields read by settings.Load when regd starts.
export const NODE_CONFIG_FIELDS = [
  { key: "REG_FACTORY_NODE_ID", label: "节点 ID", secret: false },
  { key: "REG_FACTORY_NODE_PLATFORM", label: "节点平台", secret: false },
  { key: "REG_FACTORY_NODE_KEY", label: "工作密钥", secret: true },
  { key: "REG_FACTORY_NODE_RECEIPT_KEY", label: "回执密钥", secret: true },
  { key: "REG_FACTORY_NODE_BUSINESS_KEY", label: "业务密钥", secret: true },
  { key: "REG_FACTORY_NODE_ARTIFACT_KEY", label: "资产密钥", secret: true },
];

export function isNodeConfig(key: string): boolean {
  const upper = key.toUpperCase();
  return upper.startsWith("REG_FACTORY_NODE_") || upper.startsWith("ACCOUNT_NODE_");
}
