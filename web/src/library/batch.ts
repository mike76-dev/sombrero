import { FilesError } from './api'

// describe turns an error of the file API into a sentence.
export function describe(e: unknown): string {
  if (e instanceof FilesError) {
    switch (e.message) {
      case 'already exists':
      case 'destination already exists':
        return 'Something of that name is already there.'
      case 'access denied':
        return 'This account may not do that here.'
      case 'not found':
        return 'It is no longer there.'
      case 'share unavailable':
        return 'The share is not available at the moment.'
      case 'destination is of the other kind':
        return 'A file cannot replace a folder, nor a folder a file.'
    }
    return e.message
  }
  return e instanceof Error ? e.message : String(e)
}

// what names a batch in a sentence: the item itself when it is one, the
// count otherwise.
export function what(items: { name: string }[]): string {
  return items.length === 1 ? items[0].name : `${items.length} items`
}

export type Failure<T> = { item: T; reason: string }

// runEach applies the change to each item in turn and collects what failed
// and why. A lost session ends the run early and is reported as expired.
export async function runEach<T>(items: T[], f: (item: T) => Promise<unknown>): Promise<{ failed: Failure<T>[]; expired: boolean }> {
  const failed: Failure<T>[] = []
  for (const item of items) {
    try {
      await f(item)
    } catch (e) {
      if (e instanceof FilesError && e.status === 401) return { failed, expired: true }
      failed.push({ item, reason: describe(e) })
    }
  }
  return { failed, expired: false }
}

// outcome words the result of a run: a notice where everything went through,
// a problem naming what did not otherwise.
export function outcome<T extends { name: string }>(
  items: T[],
  failed: Failure<T>[],
  done: string,
  undone: string,
): { ok: true; notice: string } | { ok: false; problem: string } {
  if (failed.length === 0) return { ok: true, notice: `${what(items)} ${items.length === 1 ? 'was' : 'were'} ${done}.` }
  if (items.length === 1) return { ok: false, problem: `${items[0].name} was not ${undone}: ${failed[0].reason}` }
  const count = failed.length === items.length ? what(items) : `${failed.length} of ${items.length} items`
  return { ok: false, problem: `${count} were not ${undone}. ${failed.map((f) => `${f.item.name}: ${f.reason}`).join(' ')}` }
}
