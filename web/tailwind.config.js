// P4 门禁加固：本文件已被 web/tsconfig.node.json 纳入 `tsc -b`（allowJs + checkJs）。
// 下面仅补充 JSDoc 类型标注，不改变任何运行时行为 / 配置语义。
/** @typedef {(path: string) => Record<string, unknown>} ThemeHelper */

/**
 * 生成「CSS 变量 + 可选 alpha」的颜色函数。
 * @param {string} variableName 形如 '--bg-rgb' 的 CSS 变量名
 * @returns {(arg: { opacityValue?: number | string }) => string}
 */
function withOpacity(variableName) {
  return ({ opacityValue }) => {
    if (opacityValue !== undefined) {
      return `rgba(var(${variableName}), ${opacityValue})`
    }
    return `rgb(var(${variableName}))`
  }
}

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        bg: withOpacity('--bg-rgb'),
        panel: withOpacity('--panel-rgb'),
        panel2: withOpacity('--panel2-rgb'),
        panel3: withOpacity('--panel3-rgb'),
        edge: withOpacity('--edge-rgb'),
        edge2: withOpacity('--edge2-rgb'),
        ink: withOpacity('--ink-rgb'),
        inkdim: withOpacity('--inkdim-rgb'),
        inkfaint: withOpacity('--inkfaint-rgb'),
        accent: withOpacity('--accent-rgb'),
        accent2: withOpacity('--accent2-rgb'),
        ok: withOpacity('--ok-rgb'),
        warn: withOpacity('--warn-rgb'),
        err: withOpacity('--err-rgb'),
      },
      /** @param {{ theme: ThemeHelper }} ctx */
      borderColor: ({ theme }) => ({
        ...theme('colors'),
        DEFAULT: withOpacity('--edge-rgb')({ opacityValue: 1 }),
      }),
      borderRadius: {
        xl2: '14px',
        xl3: '18px',
      },
      boxShadow: {
        subtle: 'none',
        card: 'none',
        glow: 'none',
        modal: 'none',
      },
      fontFamily: {
        sans: ['-apple-system', 'BlinkMacSystemFont', '"Segoe UI"', 'system-ui', '"Microsoft YaHei UI"', '"Microsoft YaHei"', '"PingFang SC"', '"Hiragino Sans GB"', 'Roboto', 'sans-serif'],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Monaco', 'Consolas', 'monospace'],
      },
    },
  },
  plugins: [],
}
