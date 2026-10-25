package web

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"burrow/internal/audit"
	"burrow/internal/files"
)

func (s *Server) handleFileRoots(w http.ResponseWriter, r *http.Request) {
	showPaths := false
	if sess, ok := SessionFrom(r.Context()); ok {
		showPaths = sess.StepupFresh(s.deps.Auth.Now(), s.deps.Auth.StepupWindow())
	}
	type root struct {
		Name     string `json:"name"`
		Writable bool   `json:"writable"`
		Path     string `json:"path,omitempty"`
	}
	roots := s.deps.Files.Resolver().Roots()
	out := make([]root, 0, len(roots))
	for _, rt := range roots {
		item := root{Name: rt.Name, Writable: rt.Writable}
		if showPaths {
			item.Path = rt.Path
		}
		out = append(out, item)
	}
	WriteJSON(w, http.StatusOK, out)
}

func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	entries, err := s.deps.Files.List(q.Get("root"), q.Get("path"), q.Get("sort"))
	if err != nil {
		s.writeFileError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, info, err := s.deps.Files.Open(q.Get("root"), q.Get("path"))
	if err != nil {
		s.writeFileError(w, err)
		return
	}
	defer f.Close()

	name := path.Base(strings.ReplaceAll(q.Get("path"), `\`, "/"))
	// Always an attachment, whatever the type: a download must never be
	// rendered by the browser in the agent's own origin.
	w.Header().Set("Content-Type", files.DownloadTypeFor(name))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment",
		map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	// ServeContent handles Range requests, which is what lets a large download
	// resume on a phone that changed network.
	w.Header().Set("Accept-Ranges", "bytes")

	s.log(audit.Entry{
		Actor: Actor(r), Action: audit.ActionFileDownload,
		Target: q.Get("root") + ":" + q.Get("path"), Result: audit.ResultOK,
		Meta: map[string]any{"bytes": info.Size()},
	})
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *Server) handleFilePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := path.Base(strings.ReplaceAll(q.Get("path"), `\`, "/"))

	contentType := files.PreviewTypeFor(name)
	if contentType == "" {
		// Not previewable: say so rather than falling back to a download, so
		// the PWA can show "download instead" rather than a broken image.
		WriteError(w, http.StatusUnsupportedMediaType, CodeBadRequest, "that file cannot be previewed")
		return
	}

	f, info, err := s.deps.Files.Open(q.Get("root"), q.Get("path"))
	if err != nil {
		s.writeFileError(w, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "no-store")

	if strings.HasPrefix(contentType, "text/plain") {
		// Text previews are capped, and the cap is applied by truncating the
		// response rather than refusing it: the first megabyte of a huge log is
		// exactly what is useful on a phone.
		if info.Size() > files.MaxPreviewTextBytes {
			w.Header().Set("X-Burrow-Truncated", "1")
			if _, err := io.CopyN(w, f, files.MaxPreviewTextBytes); err != nil && !errors.Is(err, io.EOF) {
				return
			}
			return
		}
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

func (s *Server) handleFileSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	hits, err := s.deps.Files.Search(q.Get("root"), q.Get("q"), 200, 5*time.Second)
	if err != nil {
		s.writeFileError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"results": hits})
}

func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	root, dir := q.Get("root"), q.Get("path")
	overwrite := q.Get("overwrite") == "true"

	// The body is read as a multipart stream rather than parsed into memory,
	// so a 2 GB upload does not need 2 GB of RAM.
	mr, err := r.MultipartReader()
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "expected a multipart upload")
		return
	}

	type uploaded struct {
		Name  string `json:"name"`
		Bytes int64  `json:"bytes"`
	}
	var done []uploaded

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			WriteError(w, http.StatusBadRequest, CodeBadRequest, "malformed upload")
			return
		}
		filename := part.FileName()
		if filename == "" {
			part.Close()
			continue // a non-file form field
		}
		// Only the base name is used: a browser may send a relative path, and
		// the resolver would refuse it anyway, but refusing here is clearer.
		filename = path.Base(strings.ReplaceAll(filename, `\`, "/"))
		target := filename
		if dir != "" {
			target = strings.TrimSuffix(dir, "/") + "/" + filename
		}

		n, err := s.deps.Files.Upload(root, target, part, overwrite)
		part.Close()
		if err != nil {
			s.writeFileError(w, err)
			return
		}
		s.log(audit.Entry{
			Actor: Actor(r), Action: audit.ActionFileUpload,
			Target: root + ":" + target, Result: audit.ResultOK,
			Meta: map[string]any{"bytes": n},
		})
		done = append(done, uploaded{Name: filename, Bytes: n})
	}

	if len(done) == 0 {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "no files in the upload")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "uploaded": done})
}

type mkdirRequest struct {
	Root string `json:"root"`
	Path string `json:"path"`
}

func (s *Server) handleFileMkdir(w http.ResponseWriter, r *http.Request) {
	var req mkdirRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	if err := s.deps.Files.Mkdir(req.Root, req.Path); err != nil {
		s.writeFileError(w, err)
		return
	}
	s.log(audit.Entry{
		Actor: Actor(r), Action: audit.ActionFileMkdir,
		Target: req.Root + ":" + req.Path, Result: audit.ResultOK,
	})
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type renameRequest struct {
	Root string `json:"root"`
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) handleFileRename(w http.ResponseWriter, r *http.Request) {
	var req renameRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	if err := s.deps.Files.Rename(req.Root, req.From, req.To); err != nil {
		s.writeFileError(w, err)
		return
	}
	s.log(audit.Entry{
		Actor: Actor(r), Action: audit.ActionFileRename,
		Target: req.Root + ":" + req.From, Result: audit.ResultOK,
		Meta: map[string]any{"to": req.To},
	})
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type deleteRequest struct {
	Root string `json:"root"`
	Path string `json:"path"`
}

func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	var req deleteRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	if err := s.deps.Files.Delete(req.Root, req.Path); err != nil {
		s.writeFileError(w, err)
		return
	}
	s.log(audit.Entry{
		Actor: Actor(r), Action: audit.ActionFileDelete,
		Target: req.Root + ":" + req.Path, Result: audit.ResultOK,
		Meta: map[string]any{"to": "recycle bin"},
	})
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "recycled": true})
}

// writeFileError maps resolver and operation errors to API codes.
//
// Every refusal reads the same to the client: an invalid path, a denylist hit
// and an escape attempt are all "invalid path", so a client cannot map the
// filesystem by watching which error comes back.
func (s *Server) writeFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, files.ErrNotExist):
		WriteError(w, http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, files.ErrExists):
		WriteError(w, http.StatusConflict, CodeConflict, "already exists")
	case errors.Is(err, files.ErrTooLarge):
		WriteError(w, http.StatusRequestEntityTooLarge, CodeTooLarge,
			"larger than the upload limit ("+strconv.FormatInt(s.deps.Files.MaxUploadBytes()>>20, 10)+" MB)")
	case errors.Is(err, files.ErrIsDir):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "that is a folder")
	case errors.Is(err, files.ErrNotWritable):
		WriteError(w, http.StatusForbidden, CodeForbidden, "that folder is read-only")
	case errors.Is(err, files.ErrOutsideRoot),
		errors.Is(err, files.ErrDenied),
		errors.Is(err, files.ErrReservedName),
		errors.Is(err, files.ErrInvalidPath):
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "invalid path")
	default:
		WriteError(w, http.StatusInternalServerError, CodeInternal, "the file operation failed")
	}
}
