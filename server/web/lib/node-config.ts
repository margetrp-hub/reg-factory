export function isNodeConfig(key: string): boolean {
  const upper = key.toUpperCase();
  return upper.startsWith("REG_FACTORY_NODE_") || upper.startsWith("ACCOUNT_NODE_") ||
    upper.startsWith("REG_FACTORY_ASSET_") && upper !== "REG_FACTORY_ASSET_API_KEY" || upper === "REG_FACTORY_WORK_ORIGIN";
}
