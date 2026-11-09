import catalog from "./generated-catalog.json";
import type { Script } from "./api";

/** Public navigation is generated from the same schema as executable tasks. */
export const TASK_GROUPS = catalog.groups;
export const PLATFORMS = catalog.platforms;
export const EXTERNAL_TOOLS = catalog.tools;
export const PLATFORM_TASKS = Object.fromEntries(PLATFORMS.map(platform=>[platform.slug,platform]));

export function groupOf(script: Script) {
  return TASK_GROUPS.find(group=>group.slug === script.group) || TASK_GROUPS.find(group=>group.slug === "other")!;
}
export function groupBySlug(slug: string) { return TASK_GROUPS.find(group=>group.slug === slug); }
export function allGroupSlugs() { return TASK_GROUPS.map(group=>group.slug); }
export function scriptsForPlatform(scripts: Script[], platform: string) {
  return scripts.filter(script=>script.platforms?.includes(platform) || script.platform === platform);
}
