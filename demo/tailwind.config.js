/** @type {import('tailwindcss').Config} */
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
