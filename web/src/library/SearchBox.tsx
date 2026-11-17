import { FormEvent, useEffect, useState } from 'react'
import { Icon } from './icons'
import { searchHash } from './route'

// SearchBox takes a text and goes to the search of the share for it.
export function SearchBox({ share, initial = '' }: { share: string; initial?: string }) {
  const [text, setText] = useState(initial)
  useEffect(() => setText(initial), [initial, share])

  const submit = (e: FormEvent) => {
    e.preventDefault()
    const q = text.trim()
    if (q !== '') window.location.hash = searchHash(share, q)
  }
  return (
    <form className="search-form" role="search" onSubmit={submit}>
      <Icon name="search" />
      <input type="search" value={text} onChange={(e) => setText(e.target.value)} placeholder="Search the share" aria-label="Search the share" />
    </form>
  )
}
