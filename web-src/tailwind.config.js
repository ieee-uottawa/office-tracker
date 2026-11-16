// Theme copied from ieee-webmaster/ieeeuottawa-v3 tailwind.config.mjs so the
// office tracker renders with the website's exact design system.
/** @type {import('tailwindcss').Config} */
module.exports = {
  content: ['../internal/transport/http/web/templates/**/*.html', '../internal/transport/http/web/assets/js/**/*.js'],
  darkMode: ['selector', '[data-theme="dark"]'],
  theme: {
    container: {
      center: true,
      padding: 'clamp(1rem, 2.5vw, 2rem)',
      screens: { '2xl': '86rem' },
    },
    extend: {
      borderRadius: {
        lg: 'var(--radius)',
        md: 'calc(var(--radius) - 2px)',
        sm: 'calc(var(--radius) - 4px)',
      },
      colors: {
        accent: { DEFAULT: 'hsl(var(--accent))', foreground: 'hsl(var(--accent-foreground))' },
        background: 'hsl(var(--background))',
        border: 'hsla(var(--border))',
        card: { DEFAULT: 'hsl(var(--card))', foreground: 'hsl(var(--card-foreground))' },
        destructive: { DEFAULT: 'hsl(var(--destructive))', foreground: 'hsl(var(--destructive-foreground))' },
        foreground: 'hsl(var(--foreground))',
        input: 'hsl(var(--input))',
        muted: { DEFAULT: 'hsl(var(--muted))', foreground: 'hsl(var(--muted-foreground))' },
        popover: { DEFAULT: 'hsl(var(--popover))', foreground: 'hsl(var(--popover-foreground))' },
        primary: { DEFAULT: 'hsl(var(--primary))', foreground: 'hsl(var(--primary-foreground))' },
        ring: 'hsl(var(--ring))',
        secondary: { DEFAULT: 'hsl(var(--secondary))', foreground: 'hsl(var(--secondary-foreground))' },
        success: 'hsl(var(--success))',
        error: 'hsl(var(--error))',
        warning: 'hsl(var(--warning))',
      },
      fontFamily: {
        display: ['var(--font-display)', 'sans-serif'],
        mono: ['var(--font-geist-mono)'],
        sans: ['var(--font-geist-sans)'],
      },
    },
  },
}
