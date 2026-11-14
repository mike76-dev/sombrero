import { useEffect, useState } from 'react'

export type Theme = 'light' | 'dark'

// The theme is applied to the document before the app renders, by the script
// in each page's head, and kept across the pages in localStorage.
export function useTheme(): [Theme, () => void] {
  const [theme, setTheme] = useState<Theme>(
    () => (document.documentElement.dataset.theme as Theme) || 'light',
  )
  useEffect(() => {
    document.documentElement.dataset.theme = theme
    localStorage.setItem('sombrero.theme', theme)
  }, [theme])
  return [theme, () => setTheme((t) => (t === 'light' ? 'dark' : 'light'))]
}

export function ThemeButton({ theme, toggle }: { theme: Theme; toggle: () => void }) {
  return (
    <button className="btn" onClick={toggle} title="Toggle theme">
      {theme === 'light' ? '\u{1F319} Dark mode' : '☀️ Light mode'}
    </button>
  )
}
