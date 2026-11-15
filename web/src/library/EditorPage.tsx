import { useEffect, useRef, useState } from 'react'
import { EditSession, FilesError, editSession } from './api'
import { describe } from './batch'
import { hashFor } from './route'

// EditorPage opens a file in the editor: it asks the server for a session and
// posts the access token into the editor's page, shown in a frame. The
// editor's Close button leads back to the folder.
export function EditorPage({ share, path, onExpired }: { share: string; path: string; onExpired: () => void }) {
  const [session, setSession] = useState<EditSession | null>(null)
  const [error, setError] = useState<string | null>(null)
  const form = useRef<HTMLFormElement>(null)
  const name = path.slice(path.lastIndexOf('/') + 1)
  const folder = path.slice(0, path.lastIndexOf('/')) || '/'

  useEffect(() => {
    let cancelled = false
    setSession(null)
    setError(null)
    editSession(share, path)
      .then((s) => {
        if (!cancelled) setSession(s)
      })
      .catch((e) => {
        if (cancelled) return
        if (e instanceof FilesError && e.status === 401) onExpired()
        else setError(describe(e))
      })
    return () => {
      cancelled = true
    }
  }, [share, path, onExpired])

  useEffect(() => {
    if (session) form.current?.submit()
  }, [session])

  useEffect(() => {
    if (!session) return
    const origin = new URL(session.url).origin
    const onMessage = (e: MessageEvent) => {
      if (e.origin !== origin || typeof e.data !== 'string') return
      let msg: { MessageId?: string; Values?: { Status?: string } }
      try {
        msg = JSON.parse(e.data)
      } catch {
        return
      }
      if (msg.MessageId === 'App_LoadingStatus' && msg.Values?.Status === 'Document_Loaded') {
        ;(e.source as Window | null)?.postMessage(JSON.stringify({ MessageId: 'Host_PostmessageReady' }), origin)
      }
      if (msg.MessageId === 'UI_Close') window.location.hash = hashFor(share, folder)
    }
    window.addEventListener('message', onMessage)
    return () => window.removeEventListener('message', onMessage)
  }, [session, share, folder])

  return (
    <div className="page editor-page">
      <div className="row row-spread editor-bar">
        <a href={hashFor(share, folder)}>Back to {folder === '/' ? share : folder}</a>
        <strong>{name}</strong>
      </div>
      {error ? (
        <div className="banner banner-error">{error}</div>
      ) : !session ? (
        <p className="muted">Opening the editor…</p>
      ) : (
        <>
          <form ref={form} method="post" action={session.url} target="editor" hidden>
            <input type="hidden" name="access_token" value={session.token} />
            <input type="hidden" name="access_token_ttl" value={String(session.ttl)} />
          </form>
          <iframe name="editor" title={`Editing ${name}`} className="editor-frame" allow="clipboard-read; clipboard-write" />
        </>
      )}
    </div>
  )
}
