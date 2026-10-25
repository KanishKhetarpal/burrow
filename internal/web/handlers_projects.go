package web

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"burrow/internal/audit"
	"burrow/internal/config"
	"burrow/internal/identity"
	"burrow/internal/sessions"
)

// projectsMu serialises edits to the project list, which is part of the live
// config and saved back to burrow.yaml.
var projectsMu sync.Mutex

// maxProjectCandidates caps the folder listing, so a parent with thousands of
// entries cannot produce a response the phone chokes on.
const maxProjectCandidates = 300

type projectCandidate struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Parent string `json:"parent"`
	IsGit  bool   `json:"isGit"`
	Added  bool   `json:"added"`
}

// handleProjectCandidates lists the folders directly inside each configured
// projects_parents entry, so a project can be added with a tap instead of
// typing a Windows path on a phone. Behind step-up, because it reveals the
// machine's folder layout (docs/03-SECURITY.md §3.4).
func (s *Server) handleProjectCandidates(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config()
	added := map[string]bool{}
	for _, p := range cfg.Projects {
		added[strings.ToLower(filepath.Clean(p.Path))] = true
	}

	out := []projectCandidate{}
	for _, parent := range cfg.ProjectsParents {
		entries, err := os.ReadDir(parent)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || skipProjectDir(e.Name()) {
				continue
			}
			full := filepath.Join(parent, e.Name())
			out = append(out, projectCandidate{
				Name:   e.Name(),
				Path:   full,
				Parent: parent,
				IsGit:  isGitRepo(full),
				Added:  added[strings.ToLower(full)],
			})
			if len(out) >= maxProjectCandidates {
				break
			}
		}
	}
	// Git repositories first: they are almost always what a project is.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsGit != out[j].IsGit {
			return out[i].IsGit
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	WriteJSON(w, http.StatusOK, out)
}

type addProjectRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// handleAddProject adds a folder as a project (docs/04-API.md §4). The path
// must exist and sit inside one of projects_parents, so the phone can only
// pick from folders the owner already chose at setup — it cannot point a
// Claude session at C:\Windows.
func (s *Server) handleAddProject(w http.ResponseWriter, r *http.Request) {
	var req addProjectRequest
	if !DecodeJSON(w, r, &req) {
		return
	}
	path := filepath.Clean(strings.TrimSpace(req.Path))
	if !filepath.IsAbs(path) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "path must be absolute")
		return
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		WriteError(w, http.StatusBadRequest, CodeBadRequest, "that folder does not exist")
		return
	}

	projectsMu.Lock()
	defer projectsMu.Unlock()
	cfg := s.deps.Config()

	if !underAParent(path, cfg.ProjectsParents) {
		WriteError(w, http.StatusBadRequest, CodeBadRequest,
			"that folder is not inside one of the project parent folders")
		return
	}
	for _, p := range cfg.Projects {
		if strings.EqualFold(filepath.Clean(p.Path), path) {
			WriteError(w, http.StatusConflict, CodeConflict, "that folder is already a project")
			return
		}
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = filepath.Base(path)
	}
	name = uniqueProjectName(cfg.Projects, name)

	proj := config.Project{Name: name, Path: path}
	cfg.Projects = append(cfg.Projects, proj)
	if err := config.SaveDevices(cfg); err != nil {
		cfg.Projects = cfg.Projects[:len(cfg.Projects)-1]
		WriteError(w, http.StatusInternalServerError, CodeInternal, "could not save the config")
		return
	}
	s.logProjectChange(r, "project.add", name)
	WriteJSON(w, http.StatusCreated, map[string]any{
		"id": sessions.ProjectID(proj), "name": proj.Name, "isGit": isGitRepo(path),
	})
}

// handleRemoveProject drops a project from the config. The folder itself is
// never touched.
func (s *Server) handleRemoveProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	projectsMu.Lock()
	defer projectsMu.Unlock()
	cfg := s.deps.Config()

	for i, p := range cfg.Projects {
		if sessions.ProjectID(p) != id {
			continue
		}
		before := cfg.Projects
		cfg.Projects = append(append([]config.Project{}, before[:i]...), before[i+1:]...)
		if err := config.SaveDevices(cfg); err != nil {
			cfg.Projects = before
			WriteError(w, http.StatusInternalServerError, CodeInternal, "could not save the config")
			return
		}
		s.logProjectChange(r, "project.remove", p.Name)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	WriteError(w, http.StatusNotFound, CodeNotFound, "no such project")
}

func (s *Server) logProjectChange(r *http.Request, what, name string) {
	s.log(audit.Entry{
		Actor:  Actor(r),
		Action: audit.ActionConfigChange,
		Target: "projects",
		Result: audit.ResultOK,
		IP:     identity.HostOnly(r.RemoteAddr),
		Meta:   map[string]any{"change": what, "project": name},
	})
}

// skipProjectDir leaves out folders that are never projects and some that
// should never be offered: hidden and system folders, and AppData.
func skipProjectDir(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
		return true
	}
	switch strings.ToLower(name) {
	case "appdata", "node_modules", "system volume information", "windows",
		"program files", "program files (x86)", "programdata":
		return true
	}
	return false
}

// underAParent reports whether path is strictly inside one of parents, with
// no hidden or skipped folder anywhere on the way down.
func underAParent(path string, parents []string) bool {
	for _, parent := range parents {
		rel, err := filepath.Rel(filepath.Clean(parent), path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		ok := true
		for _, seg := range strings.Split(rel, string(filepath.Separator)) {
			if skipProjectDir(seg) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func uniqueProjectName(existing []config.Project, name string) string {
	taken := map[string]bool{}
	for _, p := range existing {
		taken[strings.ToLower(p.Name)] = true
	}
	if !taken[strings.ToLower(name)] {
		return name
	}
	for n := 2; ; n++ {
		candidate := name + " " + strconv.Itoa(n)
		if !taken[strings.ToLower(candidate)] {
			return candidate
		}
	}
}
