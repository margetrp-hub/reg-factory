import type { Config } from "tailwindcss";

/** shadcn/ui 语义色，HSL 分量变量 → 支持 bg-primary/10 这类透明度 */
const alpha = (name: string) => `hsl(var(--${name}) / <alpha-value>)`;

const config: Config = {
  darkMode: ["class"],
  content: [
    "./app/**/*.{ts,tsx}",
    "./components/**/*.{ts,tsx}",
    "./lib/**/*.{ts,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        background: alpha("background"),
        foreground: alpha("foreground"),
        card: { DEFAULT: alpha("card"), foreground: alpha("card-foreground") },
        popover: { DEFAULT: alpha("popover"), foreground: alpha("popover-foreground") },
        primary: { DEFAULT: alpha("primary"), foreground: alpha("primary-foreground") },
        secondary: { DEFAULT: alpha("secondary"), foreground: alpha("secondary-foreground") },
        muted: { DEFAULT: alpha("muted"), foreground: alpha("muted-foreground") },
        accent: { DEFAULT: alpha("accent"), foreground: alpha("accent-foreground") },
        destructive: { DEFAULT: alpha("destructive"), foreground: alpha("destructive-foreground") },
        success: { DEFAULT: alpha("success"), foreground: alpha("success-foreground") },
        warning: { DEFAULT: alpha("warning"), foreground: alpha("warning-foreground") },
        brand: { DEFAULT: alpha("brand"), foreground: alpha("brand-foreground") },
        border: alpha("border"),
        input: alpha("input"),
        ring: alpha("ring"),
        sidebar: {
          DEFAULT: alpha("sidebar"),
          foreground: alpha("sidebar-foreground"),
          accent: alpha("sidebar-accent"),
          "accent-foreground": alpha("sidebar-accent-foreground"),
          border: alpha("sidebar-border"),
        },
      },
      borderRadius: {
        lg: "var(--radius)",
        md: "calc(var(--radius) - 2px)",
        sm: "calc(var(--radius) - 4px)",
      },
      fontFamily: {
        sans: ["Geist", "Geist Fallback", "ui-sans-serif", "-apple-system", "Segoe UI", "Noto Sans SC", "PingFang SC", "sans-serif"],
        mono: ["Geist Mono", "Geist Mono Fallback", "ui-monospace", "SFMono-Regular", "Menlo", "Consolas", "monospace"],
      },
      transitionTimingFunction: {
        ops: "cubic-bezier(0.2, 0, 0, 1)",
        "ops-emphasized": "cubic-bezier(0.16, 1, 0.3, 1)",
      },
    },
  },
  plugins: [],
};

export default config;
