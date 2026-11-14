import { useCallback, useEffect, useState } from "react";
import {
  Entry,
  FilesError,
  Version,
  deleteVersions,
  keepVersion,
  restore,
  versionURL,
  versions,
} from "./api";
import { ConfirmDialog, Modal } from "./Dialogs";
import { formatBytes, formatTimeExact } from "../format";

// VersionsDialog lists the previous versions of a file, with what the rights
// allow to be done with each. onChanged says the file itself changed.
export function VersionsDialog({
  share,
  entry,
  canShow,
  rights,
  onChanged,
  onExpired,
  onClose,
}: {
  share: string;
  entry: Entry;
  canShow: boolean;
  rights: { write: boolean; delete: boolean };
  onChanged: () => void;
  onExpired: () => void;
  onClose: () => void;
}) {
  const [list, setList] = useState<Version[] | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [confirm, setConfirm] = useState<
    | { kind: "restore"; v: Version }
    | { kind: "delete"; v: Version }
    | { kind: "deleteAll" }
    | null
  >(null);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let cancelled = false;
    versions(share, entry.path)
      .then((vs) => {
        if (!cancelled) setList(vs);
      })
      .catch((e) => {
        if (cancelled) return;
        if (e instanceof FilesError && e.status === 401) onExpired();
        else if (e instanceof FilesError && e.status === 404) setList([]);
        else setError(e instanceof Error ? e.message : String(e));
      });
    return () => {
      cancelled = true;
    };
  }, [share, entry.path, tick, onExpired]);

  const act = useCallback(
    async (f: () => Promise<unknown>, done: string, changed = false) => {
      setBusy(true);
      setError(null);
      try {
        await f();
        setNotice(done);
        setConfirm(null);
        setTick((t) => t + 1);
        if (changed) onChanged();
      } catch (e) {
        if (e instanceof FilesError && e.status === 401) onExpired();
        else
          setError(
            e instanceof FilesError && e.message === "access denied"
              ? "This account may not do that."
              : e instanceof Error
                ? e.message
                : String(e),
          );
      } finally {
        setBusy(false);
      }
    },
    [onChanged, onExpired],
  );

  return (
    <Modal title={`Versions of ${entry.name}`} onClose={onClose} wide>
      {notice && <div className="banner banner-success">{notice}</div>}
      {error && <div className="banner banner-error">{error}</div>}
      {!list ? (
        <p className="muted">Loading…</p>
      ) : list.length === 0 ? (
        <p className="muted">No previous versions of this file are retained.</p>
      ) : (
        <>
          <p className="muted">
            Each version is the contents the file held until the time it was
            replaced. Restoring one keeps the current contents as a version.
          </p>
          <div className="table-scroll">
            <table className="table versions">
              <thead>
                <tr>
                  <th>Last modified</th>
                  <th className="col-size">Size</th>
                  <th>Replaced</th>
                  <th className="col-actions"></th>
                </tr>
              </thead>
              <tbody>
                {list.map((v) => (
                  <tr key={v.id}>
                    <td>
                      {formatTimeExact(v.modifiedAt)}
                      {v.kept && (
                        <span className="flag flag-on version-kept">kept</span>
                      )}
                    </td>
                    <td className="col-size muted">{formatBytes(v.size)}</td>
                    <td className="muted">
                      {formatTimeExact(v.replacedAt)}
                      {v.replacedBy && (
                        <div className="version-by">by {v.replacedBy}</div>
                      )}
                    </td>
                    <td className="col-actions">
                      {canShow && (
                        <a
                          className="btn btn-small"
                          href={versionURL(
                            share,
                            entry.path,
                            { id: v.id },
                            true,
                          )}
                          target="_blank"
                          rel="noreferrer"
                        >
                          Open
                        </a>
                      )}
                      <a
                        className="btn btn-small"
                        href={versionURL(share, entry.path, { id: v.id })}
                        download={entry.name}
                      >
                        Download
                      </a>
                      {rights.write && (
                        <>
                          <button
                            className="btn btn-small"
                            disabled={busy}
                            onClick={() => setConfirm({ kind: "restore", v })}
                          >
                            Restore
                          </button>
                          <button
                            className="btn btn-small"
                            disabled={busy}
                            title={
                              v.kept
                                ? "Let the version expire again"
                                : "Keep the version beyond the limits"
                            }
                            onClick={() =>
                              act(
                                () =>
                                  keepVersion(share, entry.path, v.id, !v.kept),
                                v.kept
                                  ? "The version is released."
                                  : "The version is kept.",
                              )
                            }
                          >
                            {v.kept ? "Release" : "Keep"}
                          </button>
                        </>
                      )}
                      {rights.delete && (
                        <button
                          className="btn btn-small btn-danger"
                          disabled={busy}
                          onClick={() => setConfirm({ kind: "delete", v })}
                        >
                          Delete
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {rights.delete && (
            <div className="row modal-actions">
              <button
                className="btn btn-danger"
                disabled={busy}
                onClick={() => setConfirm({ kind: "deleteAll" })}
              >
                Delete all versions
              </button>
            </div>
          )}
        </>
      )}

      {confirm?.kind === "restore" && (
        <ConfirmDialog
          title="Restore version"
          action="Restore"
          danger={false}
          busy={busy}
          error={null}
          onClose={() => setConfirm(null)}
          onConfirm={() =>
            act(
              () => restore(share, entry.path, { id: confirm.v.id }),
              "The version is restored.",
              true,
            )
          }
        >
          <p>
            Restore the contents of <strong>{entry.name}</strong> as they were
            on {formatTimeExact(confirm.v.modifiedAt)}? The current contents are
            retained as a version.
          </p>
        </ConfirmDialog>
      )}
      {confirm?.kind === "delete" && (
        <ConfirmDialog
          title="Delete version"
          action="Delete"
          busy={busy}
          error={null}
          onClose={() => setConfirm(null)}
          onConfirm={() =>
            act(
              () => deleteVersions(share, entry.path, confirm.v.id),
              "The version is deleted.",
            )
          }
        >
          <p>
            Delete the version of <strong>{entry.name}</strong> from{" "}
            {formatTimeExact(confirm.v.modifiedAt)}? This cannot be undone.
          </p>
        </ConfirmDialog>
      )}
      {confirm?.kind === "deleteAll" && (
        <ConfirmDialog
          title="Delete all versions"
          action="Delete all"
          busy={busy}
          error={null}
          onClose={() => setConfirm(null)}
          onConfirm={() =>
            act(
              () => deleteVersions(share, entry.path),
              "All versions are deleted.",
            )
          }
        >
          <p>
            Delete every retained version of <strong>{entry.name}</strong>, kept
            ones included? The file itself stays. This cannot be undone.
          </p>
        </ConfirmDialog>
      )}
    </Modal>
  );
}
