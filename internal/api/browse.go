package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"

	"p8sos/internal/browse"
)

// File-level restore: open a backup read-only and take single files out.
// POST /backups/{name}/{id}/browse → session (202; "preparing" → "ready");
// GET /browse/{sid} → session with its filesystems ("parts");
// GET /browse/{sid}/ls?part=d0p1&path=/etc → entries;
// GET /browse/{sid}/file?part=d0p1&path=/etc/hostname → the file (download);
// DELETE /browse/{sid} → closed (also by itself after an hour).

func (s *Server) browseOn(w http.ResponseWriter) bool {
	if s.Browse == nil {
		writeErr(w, http.StatusNotFound, "not_found", "file restore isn't available on this server")
		return false
	}
	return true
}

func (s *Server) openBrowse(w http.ResponseWriter, r *http.Request) {
	if !s.browseOn(w) {
		return
	}
	b, ok := s.backupFor(w, r)
	if !ok {
		return
	}
	if b.Node != "" { // kept on a worker (only local backups have no Node)
		writeErr(w, http.StatusConflict, "conflict", fmt.Sprintf("this backup is kept on server %s: open that server's dashboard to take files out of it", b.Node))
		return
	}
	sess, err := s.Browse.Open(b, b.Owner)
	if errors.Is(err, browse.ErrBusy) {
		writeErr(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sess)
}

func (s *Server) browseFor(w http.ResponseWriter, r *http.Request) (browse.Session, bool) {
	if !s.browseOn(w) {
		return browse.Session{}, false
	}
	sess, ok := s.Browse.Get(r.PathValue("sid"))
	if !ok || !mayUse(who(r), sess.Owner) {
		writeErr(w, http.StatusNotFound, "not_found", "no such file-restore session (it closes after an hour)")
		return browse.Session{}, false
	}
	return sess, true
}

func (s *Server) getBrowse(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.browseFor(w, r); ok {
		writeJSON(w, http.StatusOK, sess)
	}
}

func (s *Server) browseErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, browse.ErrBusy):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, browse.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func (s *Server) lsBrowse(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.browseFor(w, r)
	if !ok {
		return
	}
	l, err := s.Browse.Ls(sess.ID, r.URL.Query().Get("part"), r.URL.Query().Get("path"))
	if err != nil {
		s.browseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": l})
}

func (s *Server) fileBrowse(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.browseFor(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	f, st, err := s.Browse.OpenFile(sess.ID, r.URL.Query().Get("part"), p)
	if err != nil {
		s.browseErr(w, err)
		return
	}
	defer f.Close()
	name := path.Base(path.Clean("/" + p))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, f)
}

func (s *Server) closeBrowse(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.browseFor(w, r)
	if !ok {
		return
	}
	_ = s.Browse.Close(sess.ID)
	w.WriteHeader(http.StatusNoContent)
}
