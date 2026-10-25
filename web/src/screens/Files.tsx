import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import * as api from '../api';
import { withStepUp } from '../passkey';
import { Sheet, formatBytes, timeAgo, type Toast } from '../components/ui';
import {
  IconChevronRight,
  IconFile,
  IconFolder,
  IconFolderPlus,
  IconImage,
  IconUpload,
} from '../components/icons';

const IMAGE_EXT = /\.(png|jpe?g|gif|webp|heic|bmp|svg)$/i;

export function FilesScreen(props: {
  toast: (text: string, kind?: Toast['kind']) => void;
  reportError: (e: unknown, fallback?: string) => void;
}) {
  const [roots, setRoots] = useState<api.FileRoot[]>([]);
  const [root, setRoot] = useState<string>('');
  const [path, setPath] = useState('');
  const [entries, setEntries] = useState<api.FileEntry[]>([]);
  const [loading, setLoading] = useState(false);
  const [preview, setPreview] = useState<api.FileEntry | null>(null);
  const [uploadPct, setUploadPct] = useState<number | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  const writable = roots.find((r) => r.name === root)?.writable ?? false;

  useEffect(() => {
    void (async () => {
      try {
        const rs = await api.getFileRoots();
        setRoots(rs);
        if (rs[0]) setRoot(rs[0].name);
      } catch (e) {
        props.reportError(e, 'Could not list folders');
      }
    })();
    // Roots are read once: they only change when the config does.
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const { reportError } = props;
  // Depend on reportError, not props: props is a new object on every parent
  // render (the shell ticks once a second), which would re-fetch every second.
  const load = useCallback(async () => {
    if (!root) return;
    setLoading(true);
    try {
      const res = await api.listFiles(root, path);
      setEntries(res.entries);
    } catch (e) {
      reportError(e, 'Could not open that folder');
    } finally {
      setLoading(false);
    }
  }, [root, path, reportError]);

  useEffect(() => {
    void load();
  }, [load]);

  const segments = path ? path.split('/') : [];

  async function upload(files: File[]) {
    if (files.length === 0) return;
    setUploadPct(0);
    try {
      await api.uploadFiles(root, path, files, (f) => setUploadPct(Math.round(f * 100)));
      props.toast(`Uploaded ${files.length} file${files.length === 1 ? '' : 's'}`, 'ok');
      await load();
    } catch (e) {
      props.reportError(e, 'Upload failed');
    } finally {
      setUploadPct(null);
    }
  }

  async function remove(entry: api.FileEntry) {
    if (!confirm(`Move “${entry.name}” to the Recycle Bin?`)) return;
    try {
      // Delete needs step-up; this runs Face ID and retries automatically.
      await withStepUp(() => api.deleteFile(root, join(path, entry.name)));
      props.toast('Moved to the Recycle Bin', 'ok');
      await load();
    } catch (e) {
      props.reportError(e, 'Could not delete that');
    }
  }

  async function rename(entry: api.FileEntry) {
    const next = prompt('New name', entry.name);
    if (!next || next === entry.name) return;
    try {
      await api.renameFile(root, join(path, entry.name), join(path, next));
      await load();
    } catch (e) {
      props.reportError(e, 'Could not rename that');
    }
  }

  async function newFolder() {
    const name = prompt('Folder name');
    if (!name) return;
    try {
      await api.mkdir(root, join(path, name));
      await load();
    } catch (e) {
      props.reportError(e, 'Could not create the folder');
    }
  }

  return (
    <>
      <div class="page-head">
        <h1>Files</h1>
        {writable && (
          <>
            <button type="button" class="btn-icon" aria-label="New folder" onClick={() => void newFolder()}>
              <IconFolderPlus size={20} />
            </button>
            <button
              type="button"
              class="btn-primary btn-icon"
              aria-label="Upload"
              onClick={() => fileInput.current?.click()}
            >
              <IconUpload size={20} />
            </button>
          </>
        )}
      </div>

      <div class="chip-row">
        {roots.map((r) => (
          <button
            key={r.name}
            type="button"
            class="chip"
            aria-pressed={r.name === root}
            onClick={() => {
              setRoot(r.name);
              setPath('');
            }}
          >
            {r.name}
            {!r.writable && <span class="tiny faint"> · read-only</span>}
          </button>
        ))}
      </div>

      <div class="breadcrumb">
        <button type="button" onClick={() => setPath('')}>
          {root || '…'}
        </button>
        {segments.map((seg, i) => (
          <span key={`${seg}-${i}`} style="display:contents">
            <span class="faint" aria-hidden="true">
              /
            </span>
            <button type="button" onClick={() => setPath(segments.slice(0, i + 1).join('/'))}>
              {seg}
            </button>
          </span>
        ))}
      </div>

      {writable && (
        <div>
          <input
            ref={fileInput}
            type="file"
            multiple
            style="display:none"
            onChange={(e) => {
              const input = e.target as HTMLInputElement;
              void upload(Array.from(input.files ?? []));
              input.value = '';
            }}
          />
        </div>
      )}

      {uploadPct !== null && (
        <div class="card">
          <div class="small dim" style="margin-bottom:6px">
            Uploading… {uploadPct}%
          </div>
          <div class="meter">
            <span style={`width:${uploadPct}%`} />
          </div>
        </div>
      )}

      {loading && entries.length === 0 && (
        <div class="empty">
          <span class="spin" />
        </div>
      )}
      {!loading && entries.length === 0 && (
        <div class="empty">
          <span class="tile-ico">
            <IconFolder size={26} />
          </span>
          This folder is empty.
        </div>
      )}

      {entries.length > 0 && (
        <div class="card flush">
          {entries.map((entry) => {
            const isDir = entry.type === 'dir';
            const isImg = !isDir && IMAGE_EXT.test(entry.name);
            return (
              <button
                key={entry.name}
                type="button"
                class="row"
                onClick={() => {
                  if (isDir) setPath(join(path, entry.name));
                  else setPreview(entry);
                }}
              >
                <span class={`tile-ico ${isDir ? 'gold' : isImg ? 'blue' : ''}`}>
                  {isDir ? <IconFolder size={20} /> : isImg ? <IconImage size={20} /> : <IconFile size={20} />}
                </span>
                <span class="grow">
                  <span class="truncate" style="display:block;font-weight:550">
                    {entry.name}
                  </span>
                  <span class="tiny faint">
                    {entry.type === 'file' ? `${formatBytes(entry.size)} · ` : ''}
                    {timeAgo(entry.mtime)}
                  </span>
                </span>
                {isDir && (
                  <span class="chev">
                    <IconChevronRight size={18} />
                  </span>
                )}
              </button>
            );
          })}
        </div>
      )}

      {preview && (
        <Sheet title={preview.name} onClose={() => setPreview(null)}>
          {preview.previewable ? (
            <PreviewBody root={root} path={join(path, preview.name)} name={preview.name} />
          ) : (
            <p class="dim small">No preview for this file type.</p>
          )}
          <div class="btn-row" style="margin-top:12px">
            <a
              class="btn btn-primary"
              href={api.fileURL('download', root, join(path, preview.name))}
              download={preview.name}
            >
              Download
            </a>
            {writable && (
              <>
                <button type="button" onClick={() => void rename(preview)}>
                  Rename
                </button>
                <button type="button" class="btn-danger" onClick={() => void remove(preview)}>
                  Delete
                </button>
              </>
            )}
          </div>
        </Sheet>
      )}

      <p class="tiny faint" style="margin-top:16px">
        Prefer native? Files app → Connect to Server → <span class="mono">smb://your-pc</span>
      </p>
    </>
  );
}

/** Image and PDF previews are an element; text is fetched and shown as text. */
function PreviewBody({ root, path, name }: { root: string; path: string; name: string }) {
  const url = api.fileURL('preview', root, path);
  const ext = name.slice(name.lastIndexOf('.')).toLowerCase();

  if (['.jpg', '.jpeg', '.png', '.gif', '.webp'].includes(ext)) {
    return <img src={url} alt={name} style="max-width:100%;border-radius:var(--radius-sm)" />;
  }
  if (ext === '.pdf') {
    return <iframe src={url} title={name} style="width:100%;height:60vh;border:0" />;
  }
  return <TextPreview url={url} />;
}

function TextPreview({ url }: { url: string }) {
  const [text, setText] = useState<string | null>(null);
  useEffect(() => {
    void (async () => {
      try {
        const res = await fetch(url, { credentials: 'same-origin' });
        setText(await res.text());
      } catch {
        setText('(could not load)');
      }
    })();
  }, [url]);
  if (text === null) return <p class="dim small">Loading…</p>;
  return (
    <pre class="mono" style="white-space:pre-wrap;word-break:break-word;max-height:55vh;overflow:auto">
      {text}
    </pre>
  );
}

function join(base: string, name: string): string {
  return base ? `${base}/${name}` : name;
}
