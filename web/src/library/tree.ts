import { Incoming } from './Uploads'

// A Tree is what a drop or a picker brings in: the files, each with the
// folder it goes into, and the folders to make first, parents before children.
export interface Tree {
  files: Incoming[]
  folders: string[]
}

export function join(folder: string, name: string): string {
  return (folder === '/' ? '' : folder) + '/' + name
}

// gather walks a drop into the folder. A dropped folder is opened through the
// entry the browser offers for it, which its file list does not open; the
// entries are taken while the drop event lasts, before anything is awaited.
export async function gather(dt: DataTransfer, folder: string): Promise<Tree> {
  const tree: Tree = { files: [], folders: [] }
  const entries = Array.from(dt.items ?? []).map((item) => (item.kind === 'file' ? item.webkitGetAsEntry() : null))
  if (entries.length === 0 || entries.some((e) => e === null)) {
    tree.files = Array.from(dt.files).map((file) => ({ file, folder }))
    return tree
  }
  for (const entry of entries) await walk(entry!, folder, tree)
  return tree
}

async function walk(entry: FileSystemEntry, folder: string, tree: Tree): Promise<void> {
  if (entry.isFile) {
    const file = await new Promise<File>((resolve, reject) => (entry as FileSystemFileEntry).file(resolve, reject))
    tree.files.push({ file, folder })
    return
  }
  if (!entry.isDirectory) return
  const path = join(folder, entry.name)
  tree.folders.push(path)
  // A reader hands the entries out in batches and an empty one at the end.
  const reader = (entry as FileSystemDirectoryEntry).createReader()
  for (;;) {
    const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => reader.readEntries(resolve, reject))
    if (batch.length === 0) return
    for (const e of batch) await walk(e, path, tree)
  }
}

// fromPicker takes the files of a picker into the folder. A picked folder
// gives each file its path within it, from which the folders are read.
export function fromPicker(files: File[], folder: string): Tree {
  const tree: Tree = { files: [], folders: [] }
  const seen = new Set<string>()
  for (const file of files) {
    let at = folder
    for (const part of file.webkitRelativePath.split('/').slice(0, -1)) {
      at = join(at, part)
      if (!seen.has(at)) {
        seen.add(at)
        tree.folders.push(at)
      }
    }
    tree.files.push({ file, folder: at })
  }
  return tree
}
