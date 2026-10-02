package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"src.solsynth.dev/solsynth/maidcafe/internal/config"
)

// The file-management API: the operations a browser-based MaidKit build
// cannot perform over SFTP. It is opt-in ([daemon.files]) and every path must
// be absolute and inside one of the configured roots.
//
// Two layers of containment guard a request. The daemon resolves the request
// path lexically against the roots and refuses anything outside them, so a
// client cannot even name a file it may not touch. The I/O itself then runs
// through os.Root, which refuses any access — including one reached through a
// symbolic link — that resolves outside the root, so a link planted inside a
// root cannot be used to escape it. Both checks are needed: the lexical one
// gives a precise refusal, and the kernel-enforced one survives a race
// between the check and the operation.
//
// Operations run as the daemon's own account. The API never elevates, so it
// can touch exactly what that account can touch; a root the daemon cannot
// read is a root the API cannot serve.

// File API action names. They are stable slugs (mirroring the native
// operation naming) because the stdio transport dispatches by name.
const (
	fileActionRoots  = "files.roots"
	fileActionList   = "files.list"
	fileActionStat   = "files.stat"
	fileActionRead   = "files.read"
	fileActionWrite  = "files.write"
	fileActionMkdir  = "files.mkdir"
	fileActionMove   = "files.move"
	fileActionCopy   = "files.copy"
	fileActionDelete = "files.delete"
)

// File API audit sources: the transport a file operation arrived on.
const (
	fileSourceHTTP  = "http"
	fileSourceStdio = "stdio"
)

// fileActionError is a request-level failure with the HTTP status it maps to,
// mirroring requestError for script runs.
type fileActionError struct {
	status  int
	message string
}

// isFileActionSlug reports whether [name] is a file API action. The stdio
// transport dispatches by name, mirroring native operations.
func isFileActionSlug(name string) bool {
	switch name {
	case fileActionRoots, fileActionList, fileActionStat, fileActionRead,
		fileActionWrite, fileActionMkdir, fileActionMove, fileActionCopy, fileActionDelete:
		return true
	}
	return false
}

// runStdioFileAction executes one file action requested over the stdio pipe.
// The parameters arrive as the JSON body object; the audit entry records the
// transport ("stdio") as the actor, because the pipe itself carries no
// identity.
func (a *App) runStdioFileAction(ctx context.Context, action string, body []byte) (any, *requestError) {
	policy, policyErr := a.filePolicyFrom()
	if policyErr != nil {
		return nil, &requestError{status: policyErr.status, message: policyErr.message}
	}
	var req fileActionRequest
	if len(body) > 0 {
		var values map[string]any
		if err := json.Unmarshal(body, &values); err != nil {
			return nil, &requestError{status: http.StatusBadRequest, message: "invalid JSON body"}
		}
		if err := fileBodyFromValues(values, &req, action); err != nil {
			a.recordFileOp(fileSourceStdio, action, "stdio", req.Path, false, err, time.Now())
			return nil, &requestError{status: err.status, message: err.message}
		}
	}
	result, err := a.runFileAction(ctx, policy, fileSourceStdio, action, "stdio", req)
	if err != nil {
		return nil, &requestError{status: err.status, message: err.message}
	}
	return result, nil
}

func fileError(status int, format string, args ...any) *fileActionError {
	return &fileActionError{status: status, message: fmt.Sprintf(format, args...)}
}

// filePolicy is the resolved [config.FilesConfig]: the effective limits and
// the cleaned root allowlist. A nil policy means the API is off, which every
// entry point reports as a refusal rather than an empty success.
type filePolicy struct {
	// secret is the credential this API answers to: the dedicated file
	// secret when configured, metricsSecret otherwise. Resolved here so a
	// handler and the stdio entry point cannot disagree about it.
	secret     string
	allowWrite bool
	maxRead    int64
	maxWrite   int64
	maxList    int
	// roots is the cleaned root allowlist, each with its privilege policy. It
	// is not named "allowed" so the listing method can keep that name.
	roots []fileRoot
}

// fileRoot is one configured root: the cleaned directory plus how writes to it
// are authorized. A privileged root is written through maidkit-priv, whose own
// profile file decides which directory that profile name means; the daemon
// only carries the name.
type fileRoot struct {
	path       string
	privileged bool
	profile    string
}

// newFilePolicy resolves the configured limits and cleans the roots. The
// roots are validated at config load (absolute, existing directories), so
// resolving is pure string work and is cheap enough to do per request — which
// is also what keeps a hot reload honest.
func newFilePolicy(cfg config.FilesConfig) *filePolicy {
	if !cfg.Enabled {
		return nil
	}
	policy := &filePolicy{
		secret:     cfg.Secret,
		allowWrite: cfg.AllowWrite,
		maxRead:    cfg.MaxReadBytes,
		maxWrite:   cfg.MaxWriteBytes,
		maxList:    cfg.MaxListEntries,
		roots:      make([]fileRoot, 0, len(cfg.Roots)),
	}
	if policy.maxRead <= 0 {
		policy.maxRead = config.FilesDefaultMaxReadBytes
	}
	if policy.maxWrite <= 0 {
		policy.maxWrite = config.FilesDefaultMaxWriteBytes
	}
	if policy.maxList <= 0 {
		policy.maxList = config.FilesDefaultMaxListEntries
	}
	for _, root := range cfg.Roots {
		policy.roots = append(policy.roots, fileRoot{
			path:       filepath.Clean(root.Path),
			privileged: root.Privileged,
			profile:    root.Profile,
		})
	}
	return policy
}

// fileTarget is one request path resolved against the policy: the root it
// lives in, the root-relative path os.Root and the helper take, and the clean
// absolute path the client sees in responses. [privileged] and [profile] come
// from the root, so a mutation knows how it must be authorized.
type fileTarget struct {
	rootPath   string
	rel        string
	path       string
	privileged bool
	profile    string
}

// fileEntry is one directory entry or stat record. Mode carries the unix
// permission bits only; the kind of entry is Type.
type fileEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	ModifiedAt time.Time `json:"modified_at"`
	// LinkTarget is the raw symlink destination; TargetType is the kind the
	// destination resolves to when it stays inside the root (empty when the
	// link is broken or points outside).
	LinkTarget string `json:"link_target,omitempty"`
	TargetType string `json:"target_type,omitempty"`
}

// fileRootInfo is one entry of the roots response. A client needs to know
// which roots are writable only through the privileged helper, because the
// operations the helper does not implement (move, copy) are refused there and
// it should not offer them.
type fileRootInfo struct {
	Path       string `json:"path"`
	Privileged bool   `json:"privileged"`
	Profile    string `json:"profile,omitempty"`
}

type fileRootsResult struct {
	Roots    []fileRootInfo `json:"roots"`
	Writable bool           `json:"writable"`
}

type fileListResult struct {
	Path      string      `json:"path"`
	Entries   []fileEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

type fileReadResult struct {
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
	Size    int64  `json:"size"`
	Content string `json:"content"` // base64
}

type fileWriteResult struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type filePathResult struct {
	Path string `json:"path"`
}

type fileMoveResult struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// fileActionRequest is the request shape shared by the stdio actions and the
// JSON mutation routes. Fields not relevant to an action are ignored.
type fileActionRequest struct {
	Path      string `json:"path"`
	From      string `json:"from"`
	To        string `json:"to"`
	Parents   bool   `json:"parents"`
	Recursive bool   `json:"recursive"`
	Overwrite bool   `json:"overwrite"`
	Follow    *bool  `json:"follow"`
	Offset    int64  `json:"offset"`
	Limit     int64  `json:"limit"`
	Content   string `json:"content"` // base64
}

// resolve maps a request path onto a policy root. The path must be absolute
// and lexically inside a root; the longest matching root wins so nested roots
// resolve to the most specific one.
func (p *filePolicy) resolve(raw string) (fileTarget, *fileActionError) {
	if strings.ContainsRune(raw, 0) {
		return fileTarget{}, fileError(http.StatusBadRequest, "path must not contain NUL")
	}
	if !filepath.IsAbs(raw) {
		return fileTarget{}, fileError(http.StatusBadRequest, "path must be absolute")
	}
	clean := filepath.Clean(raw)
	var best *fileRoot
	for i := range p.roots {
		root := &p.roots[i]
		// The longest matching root wins, so a nested root resolves to the
		// most specific privilege policy rather than to its parent's.
		if !pathInside(root.path, clean) {
			continue
		}
		if best == nil || len(root.path) > len(best.path) {
			best = root
		}
	}
	if best == nil {
		return fileTarget{}, fileError(http.StatusForbidden, "path is outside the allowed roots")
	}
	rel := strings.TrimPrefix(clean, best.path)
	rel = strings.TrimPrefix(rel, string(os.PathSeparator))
	if rel == "" {
		rel = "."
	}
	return fileTarget{
		rootPath:   best.path,
		rel:        rel,
		path:       clean,
		privileged: best.privileged,
		profile:    best.profile,
	}, nil
}

// pathInside reports whether [target] is [root] itself or sits beneath it.
func pathInside(root, target string) bool {
	if target == root {
		return true
	}
	if root == string(os.PathSeparator) {
		return true
	}
	return strings.HasPrefix(target, root+string(os.PathSeparator))
}

// open opens the target's root for the duration of one operation. The caller
// closes it. os.Root confines every access to the root, so the operation
// cannot be redirected outside it by a symlink.
func (t fileTarget) open() (*os.Root, *fileActionError) {
	root, err := os.OpenRoot(t.rootPath)
	if err != nil {
		return nil, fileError(http.StatusInternalServerError, "open root %s: %v", t.rootPath, err)
	}
	return root, nil
}

// filePolicyFrom returns the current policy from the reloadable snapshot, or
// the refusal every entry point shares when the API is off.
func (a *App) filePolicyFrom() (*filePolicy, *fileActionError) {
	policy := newFilePolicy(a.rt.Load().files)
	if policy == nil {
		return nil, fileError(http.StatusForbidden, "file API is disabled")
	}
	return policy, nil
}

// recordFileOp appends one file operation's audit entry. The entry names the
// action and carries the resolved path in Target, so the audit log answers
// "who touched which file" without the stdout/stderr fields scripting runs
// use.
func (a *App) recordFileOp(source, action, invokedBy, target string, privileged bool, err *fileActionError, started time.Time) {
	if a.audit == nil {
		return
	}
	entry := auditEntry{
		Timestamp:  time.Now().UTC(),
		Name:       action,
		Source:     source,
		InvokedBy:  invokedBy,
		Target:     target,
		Privileged: privileged,
		OK:         err == nil,
		DurationMS: time.Since(started).Milliseconds(),
	}
	if err != nil {
		entry.Error = err.message
	}
	a.audit.Record(entry)
}

// rootInfos reports the configured roots so a client can seed its browser with
// the directories it may show, including which of them need the privileged
// helper.
func (p *filePolicy) rootInfos() fileRootsResult {
	result := fileRootsResult{Roots: make([]fileRootInfo, 0, len(p.roots)), Writable: p.allowWrite}
	for _, root := range p.roots {
		result.Roots = append(result.Roots, fileRootInfo{
			Path: root.path, Privileged: root.privileged, Profile: root.profile,
		})
	}
	return result
}

// list reads one directory. A directory larger than the listing cap is
// reported truncated rather than silently shortened.
func (p *filePolicy) list(t fileTarget) (fileListResult, *fileActionError) {
	root, openErr := t.open()
	if openErr != nil {
		return fileListResult{}, openErr
	}
	defer root.Close()
	info, err := root.Stat(t.rel)
	if err != nil {
		return fileListResult{}, fileStatus(err)
	}
	if !info.IsDir() {
		return fileListResult{}, fileError(http.StatusBadRequest, "not a directory")
	}
	dir, err := root.Open(t.rel)
	if err != nil {
		return fileListResult{}, fileStatus(err)
	}
	defer dir.Close()
	entries, truncated, err := readDirLimited(dir, p.maxList)
	if err != nil {
		return fileListResult{}, fileStatus(err)
	}
	result := fileListResult{Path: t.path, Entries: make([]fileEntry, 0, len(entries)), Truncated: truncated}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// An entry that vanished between the read and the stat is
			// skipped; a listing is a snapshot, not a transaction.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fileListResult{}, fileStatus(err)
		}
		item := fileEntry{
			Name:       entry.Name(),
			Path:       filepath.Join(t.path, entry.Name()),
			Type:       fileType(info.Mode()),
			Size:       info.Size(),
			Mode:       uint32(info.Mode().Perm()),
			ModifiedAt: info.ModTime().UTC(),
		}
		if item.Type == "symlink" {
			fillLinkTarget(root, filepath.Join(t.rel, entry.Name()), &item)
		}
		result.Entries = append(result.Entries, item)
	}
	sort.Slice(result.Entries, func(i, j int) bool { return result.Entries[i].Name < result.Entries[j].Name })
	return result, nil
}

// stat reports one path. [follow] resolves a symlink to its target — a link
// that is broken or leaves the root then fails, like SFTP's follow stat —
// while without it the link itself is reported, which is what a client needs
// before it deletes or retargets one.
func (p *filePolicy) stat(t fileTarget, follow bool) (fileEntry, *fileActionError) {
	root, openErr := t.open()
	if openErr != nil {
		return fileEntry{}, openErr
	}
	defer root.Close()
	info, err := root.Lstat(t.rel)
	if err != nil {
		return fileEntry{}, fileStatus(err)
	}
	if follow && info.Mode()&fs.ModeSymlink != 0 {
		resolved, err := root.Stat(t.rel)
		if err != nil {
			return fileEntry{}, fileStatus(err)
		}
		info = resolved
	}
	entry := fileEntry{
		Name:       filepath.Base(t.path),
		Path:       t.path,
		Type:       fileType(info.Mode()),
		Size:       info.Size(),
		Mode:       uint32(info.Mode().Perm()),
		ModifiedAt: info.ModTime().UTC(),
	}
	if entry.Type == "symlink" {
		fillLinkTarget(root, t.rel, &entry)
	}
	return entry, nil
}

// fillLinkTarget records where a symlink points and what it resolves to. A
// destination that leaves the root is left empty: the client shows the link
// but cannot follow it through this API.
func fillLinkTarget(root *os.Root, rel string, entry *fileEntry) {
	target, err := root.Readlink(rel)
	if err != nil {
		return
	}
	entry.LinkTarget = target
	if resolved, err := root.Stat(rel); err == nil {
		entry.TargetType = fileType(resolved.Mode())
	}
}

// read returns up to [limit] bytes at [offset] from one file, base64-encoded
// for the JSON transports. The HTTP content route streams the same window
// through [filePolicy.openReadWindow] instead, so both enforce one set of
// checks.
func (p *filePolicy) read(t fileTarget, offset, limit int64) (fileReadResult, *fileActionError) {
	file, size, window, err := p.openReadWindow(t, offset, limit)
	if err != nil {
		return fileReadResult{}, err
	}
	defer file.Close()
	buf := make([]byte, window)
	if _, readErr := io.ReadFull(file, buf); readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return fileReadResult{}, fileStatus(readErr)
	}
	return fileReadResult{
		Path:    t.path,
		Offset:  offset,
		Size:    size,
		Content: base64.StdEncoding.EncodeToString(buf),
	}, nil
}

// openReadWindow validates a read request and returns the opened file, the
// file's total size and how many bytes the window covers. The caller closes
// the file.
//
// An explicit limit may be shorter than the file; without one, a file larger
// than the read cap is refused (413) instead of silently truncated, so a
// client cannot mistake a partial read for the whole file. Either way the
// response carries the total size, so a client can page the rest.
func (p *filePolicy) openReadWindow(t fileTarget, offset, limit int64) (*os.File, int64, int64, *fileActionError) {
	root, openErr := t.open()
	if openErr != nil {
		return nil, 0, 0, openErr
	}
	defer root.Close()
	file, err := root.Open(t.rel)
	if err != nil {
		return nil, 0, 0, fileStatus(err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, 0, fileStatus(err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, 0, 0, fileError(http.StatusBadRequest, "not a regular file")
	}
	size := info.Size()
	if offset < 0 || offset > size {
		file.Close()
		return nil, 0, 0, fileError(http.StatusRequestedRangeNotSatisfiable, "offset %d is outside the file (%d bytes)", offset, size)
	}
	if limit < 0 {
		file.Close()
		return nil, 0, 0, fileError(http.StatusBadRequest, "limit must not be negative")
	}
	if limit > p.maxRead {
		file.Close()
		return nil, 0, 0, fileError(http.StatusRequestEntityTooLarge, "limit exceeds maxReadBytes (%d)", p.maxRead)
	}
	remaining := size - offset
	switch {
	case limit == 0 && remaining > p.maxRead:
		file.Close()
		return nil, 0, 0, fileError(http.StatusRequestEntityTooLarge,
			"file is larger than maxReadBytes (%d); read it in windows with offset and limit", p.maxRead)
	case limit == 0 || limit > remaining:
		limit = remaining
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return nil, 0, 0, fileStatus(err)
	}
	return file, size, limit, nil
}

// write creates or truncates [t] with [data]. The parent directory must
// already exist; creating it is a separate, explicit mkdir.
func (p *filePolicy) write(t fileTarget, data []byte) (fileWriteResult, *fileActionError) {
	if !p.allowWrite {
		return fileWriteResult{}, fileError(http.StatusForbidden, "file API is read-only")
	}
	if t.privileged {
		// Reaching here would mean the dispatch forgot to route a privileged
		// root through the helper, which would silently write as the daemon
		// account instead. Refuse rather than do the wrong thing.
		return fileWriteResult{}, fileError(http.StatusInternalServerError, "internal error: privileged root not routed through the helper")
	}
	if t.rel == "." {
		return fileWriteResult{}, fileError(http.StatusBadRequest, "refusing to write a configured root")
	}
	if int64(len(data)) > p.maxWrite {
		return fileWriteResult{}, fileError(http.StatusRequestEntityTooLarge, "body exceeds maxWriteBytes (%d)", p.maxWrite)
	}
	root, openErr := t.open()
	if openErr != nil {
		return fileWriteResult{}, openErr
	}
	defer root.Close()
	if info, err := root.Stat(t.rel); err == nil && info.IsDir() {
		return fileWriteResult{}, fileError(http.StatusConflict, "path is a directory")
	}
	file, err := root.OpenFile(t.rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fileWriteResult{}, fileStatus(err)
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return fileWriteResult{}, fileStatus(err)
	}
	return fileWriteResult{Path: t.path, Size: int64(len(data))}, nil
}

// mkdir creates one directory, or the missing parents when [parents] is set.
func (p *filePolicy) mkdir(t fileTarget, parents bool) (filePathResult, *fileActionError) {
	if !p.allowWrite {
		return filePathResult{}, fileError(http.StatusForbidden, "file API is read-only")
	}
	if t.privileged {
		return filePathResult{}, fileError(http.StatusInternalServerError, "internal error: privileged root not routed through the helper")
	}
	if t.rel == "." {
		return filePathResult{}, fileError(http.StatusConflict, "path already exists")
	}
	root, openErr := t.open()
	if openErr != nil {
		return filePathResult{}, openErr
	}
	defer root.Close()
	var err error
	if parents {
		err = root.MkdirAll(t.rel, 0o755)
	} else {
		err = root.Mkdir(t.rel, 0o755)
	}
	if err != nil {
		return filePathResult{}, fileStatus(err)
	}
	return filePathResult{Path: t.path}, nil
}

// move renames a path inside one root. A cross-root move is refused: the two
// roots may be different filesystems, and pretending otherwise would replace
// a rename with a copy the caller did not ask for.
func (p *filePolicy) move(from, to fileTarget) (fileMoveResult, *fileActionError) {
	if !p.allowWrite {
		return fileMoveResult{}, fileError(http.StatusForbidden, "file API is read-only")
	}
	if from.rootPath != to.rootPath {
		return fileMoveResult{}, fileError(http.StatusBadRequest, "move must stay inside one configured root")
	}
	if from.rel == "." || to.rel == "." {
		return fileMoveResult{}, fileError(http.StatusBadRequest, "refusing to move a configured root")
	}
	root, openErr := from.open()
	if openErr != nil {
		return fileMoveResult{}, openErr
	}
	defer root.Close()
	if _, err := root.Lstat(from.rel); err != nil {
		return fileMoveResult{}, fileStatus(err)
	}
	if err := root.Rename(from.rel, to.rel); err != nil {
		return fileMoveResult{}, fileStatus(err)
	}
	return fileMoveResult{From: from.path, To: to.path}, nil
}

// copy duplicates a file or a whole directory tree inside one root. An
// existing destination is refused unless [overwrite] is set, so a client
// cannot clobber a file it did not intend to replace.
func (p *filePolicy) copy(from, to fileTarget, overwrite bool) (fileMoveResult, *fileActionError) {
	if !p.allowWrite {
		return fileMoveResult{}, fileError(http.StatusForbidden, "file API is read-only")
	}
	if from.rootPath != to.rootPath {
		return fileMoveResult{}, fileError(http.StatusBadRequest, "copy must stay inside one configured root")
	}
	if from.rel == "." || to.rel == "." {
		return fileMoveResult{}, fileError(http.StatusBadRequest, "refusing to copy a configured root")
	}
	if from.rel == to.rel {
		return fileMoveResult{}, fileError(http.StatusBadRequest, "source and destination are the same path")
	}
	if strings.HasPrefix(to.rel, from.rel+string(os.PathSeparator)) {
		return fileMoveResult{}, fileError(http.StatusBadRequest, "destination is inside the source")
	}
	root, openErr := from.open()
	if openErr != nil {
		return fileMoveResult{}, openErr
	}
	defer root.Close()
	info, err := root.Lstat(from.rel)
	if err != nil {
		return fileMoveResult{}, fileStatus(err)
	}
	if _, err := root.Lstat(to.rel); err == nil {
		if !overwrite {
			return fileMoveResult{}, fileError(http.StatusConflict, "destination already exists")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fileMoveResult{}, fileStatus(err)
	}
	if info.IsDir() {
		if err := copyFileTree(root, from.rel, to.rel, info.Mode().Perm(), overwrite); err != nil {
			return fileMoveResult{}, fileStatus(err)
		}
	} else if err := copyFileContents(root, from.rel, to.rel, info.Mode().Perm()); err != nil {
		return fileMoveResult{}, fileStatus(err)
	}
	return fileMoveResult{From: from.path, To: to.path}, nil
}

// copyFileTree recursively duplicates a directory. Symbolic links inside the
// tree are recreated as links; a link whose destination cannot be read inside
// the root fails the copy instead of being silently dropped.
func copyFileTree(root *os.Root, fromRel, toRel string, perm fs.FileMode, overwrite bool) error {
	if err := root.MkdirAll(toRel, perm|0o700); err != nil {
		return err
	}
	source, err := root.Open(fromRel)
	if err != nil {
		return err
	}
	defer source.Close()
	entries, err := source.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childFrom := filepath.Join(fromRel, entry.Name())
		childTo := filepath.Join(toRel, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := copyFileTree(root, childFrom, childTo, info.Mode().Perm(), overwrite); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := root.Readlink(childFrom)
			if err != nil {
				return fmt.Errorf("read link %s: %w", childFrom, err)
			}
			if _, err := root.Lstat(childTo); err == nil && overwrite {
				if err := root.Remove(childTo); err != nil {
					return err
				}
			}
			if err := root.Symlink(target, childTo); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := copyFileContents(root, childFrom, childTo, info.Mode().Perm()); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFileContents streams one file. The copy is bounded by memory, never by
// the read cap: a download limit exists to bound one HTTP response, not to
// stop the daemon from duplicating a large file it can already read.
func copyFileContents(root *os.Root, fromRel, toRel string, perm fs.FileMode) error {
	source, err := root.Open(fromRel)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := root.OpenFile(toRel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer destination.Close()
	if _, err := io.Copy(destination, source); err != nil {
		return err
	}
	return destination.Sync()
}

// delete removes one path, or the whole tree beneath it when [recursive] is
// set. A configured root is never deletable through the API.
func (p *filePolicy) delete(t fileTarget, recursive bool) (filePathResult, *fileActionError) {
	if !p.allowWrite {
		return filePathResult{}, fileError(http.StatusForbidden, "file API is read-only")
	}
	if t.privileged {
		return filePathResult{}, fileError(http.StatusInternalServerError, "internal error: privileged root not routed through the helper")
	}
	if t.rel == "." {
		return filePathResult{}, fileError(http.StatusBadRequest, "refusing to delete a configured root")
	}
	root, openErr := t.open()
	if openErr != nil {
		return filePathResult{}, openErr
	}
	defer root.Close()
	if _, err := root.Lstat(t.rel); err != nil {
		return filePathResult{}, fileStatus(err)
	}
	if recursive {
		if err := root.RemoveAll(t.rel); err != nil {
			return filePathResult{}, fileStatus(err)
		}
	} else if err := root.Remove(t.rel); err != nil {
		return filePathResult{}, fileStatus(err)
	}
	return filePathResult{Path: t.path}, nil
}

// readDirLimited reads at most [limit]+1 entries, reporting whether more
// remain. The extra entry is what distinguishes a directory that exactly
// fills the window from one that overflows it.
func readDirLimited(dir *os.File, limit int) ([]fs.DirEntry, bool, error) {
	entries := make([]fs.DirEntry, 0, min(limit, 256))
	for len(entries) <= limit {
		batch, err := dir.ReadDir(min(256, limit+1-len(entries)))
		entries = append(entries, batch...)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if err != nil {
			return nil, false, err
		}
		if len(batch) == 0 {
			break
		}
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}

// fileType names a filesystem entry kind: the client's file manager paints
// directories, files and links differently and refuses to open anything else.
func fileType(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "directory"
	case mode.IsRegular():
		return "file"
	default:
		return "other"
	}
}

// fileStatus maps a filesystem error to the status and message a client sees.
// A refusal from os.Root ("path escapes from parent") is a policy answer, not
// an internal failure, so it becomes a 403 like the lexical root check.
func fileStatus(err error) *fileActionError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fileError(http.StatusNotFound, "no such file or directory")
	case errors.Is(err, fs.ErrPermission):
		return fileError(http.StatusForbidden, "permission denied")
	case errors.Is(err, fs.ErrExist):
		return fileError(http.StatusConflict, "already exists")
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		return fileError(http.StatusConflict, "directory is not empty")
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.EINVAL):
		return fileError(http.StatusBadRequest, "invalid operation for this path")
	case errors.Is(err, syscall.EXDEV):
		return fileError(http.StatusBadRequest, "cross-device operation is not supported")
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && strings.Contains(pathErr.Err.Error(), "escapes from parent") {
		return fileError(http.StatusForbidden, "path escapes the allowed roots")
	}
	return fileError(http.StatusInternalServerError, "file operation failed: %v", err)
}

// runFileAction executes one file action against [policy], recording a single
// audit entry. Both transports share it so stdio and HTTP cannot drift apart
// in what they accept or what they refuse.
func (a *App) runFileAction(ctx context.Context, policy *filePolicy, source, action, invokedBy string, req fileActionRequest) (any, *fileActionError) {
	started := time.Now()
	target := req.Path
	if action == fileActionMove || action == fileActionCopy {
		target = req.From + " -> " + req.To
	}
	result, privileged, err := a.dispatchFileAction(ctx, policy, action, req)
	a.recordFileOp(source, action, invokedBy, target, privileged, err, started)
	return result, err
}

// dispatchFileAction resolves and runs one action. It reports whether the
// action went through the privileged helper, which the audit entry records so
// an operator can answer "which file changes needed root".
//
// A privileged root is routed to the helper for the three operations it
// implements. move and copy are refused there rather than attempted: a rename
// inside a root-owned directory needs root too, the helper does not implement
// it, and silently falling back to the daemon account would turn a supported
// call into a permission error at best and a partial copy at worst.
func (a *App) dispatchFileAction(ctx context.Context, policy *filePolicy, action string, req fileActionRequest) (any, bool, *fileActionError) {
	switch action {
	case fileActionRoots:
		return policy.rootInfos(), false, nil
	case fileActionList:
		target, err := policy.resolve(req.Path)
		if err != nil {
			return nil, false, err
		}
		result, err := policy.list(target)
		return result, false, err
	case fileActionStat:
		target, err := policy.resolve(req.Path)
		if err != nil {
			return nil, false, err
		}
		follow := true
		if req.Follow != nil {
			follow = *req.Follow
		}
		entry, err := policy.stat(target, follow)
		return entry, false, err
	case fileActionRead:
		target, err := policy.resolve(req.Path)
		if err != nil {
			return nil, false, err
		}
		result, err := policy.read(target, req.Offset, req.Limit)
		return result, false, err
	case fileActionWrite:
		target, err := policy.resolve(req.Path)
		if err != nil {
			return nil, false, err
		}
		data, decodeErr := base64.StdEncoding.DecodeString(req.Content)
		if decodeErr != nil {
			return nil, false, fileError(http.StatusBadRequest, "content must be base64")
		}
		result, privileged, err := a.writeFileData(ctx, policy, target, data)
		return result, privileged, err
	case fileActionMkdir:
		target, err := policy.resolve(req.Path)
		if err != nil {
			return nil, false, err
		}
		if target.privileged {
			if policyErr := checkPolicyWrite(policy, target, 0); policyErr != nil {
				return nil, false, policyErr
			}
			if privErr := a.privMkdir(ctx, target, privMkdirMode); privErr != nil {
				return nil, true, privErr
			}
			return filePathResult{Path: target.path}, true, nil
		}
		result, err := policy.mkdir(target, req.Parents)
		return result, false, err
	case fileActionMove:
		from, err := policy.resolve(req.From)
		if err != nil {
			return nil, false, err
		}
		to, err := policy.resolve(req.To)
		if err != nil {
			return nil, false, err
		}
		if from.privileged || to.privileged {
			return nil, false, errPrivilegedMoveOrCopy
		}
		result, err := policy.move(from, to)
		return result, false, err
	case fileActionCopy:
		from, err := policy.resolve(req.From)
		if err != nil {
			return nil, false, err
		}
		to, err := policy.resolve(req.To)
		if err != nil {
			return nil, false, err
		}
		if from.privileged || to.privileged {
			return nil, false, errPrivilegedMoveOrCopy
		}
		result, err := policy.copy(from, to, req.Overwrite)
		return result, false, err
	case fileActionDelete:
		target, err := policy.resolve(req.Path)
		if err != nil {
			return nil, false, err
		}
		if target.privileged {
			if policyErr := checkPolicyWrite(policy, target, 0); policyErr != nil {
				return nil, false, policyErr
			}
			if req.Recursive {
				return nil, false, fileError(http.StatusNotImplemented,
					"a privileged root only removes single files; recursive delete is not implemented")
			}
			if privErr := a.privRemove(ctx, target); privErr != nil {
				return nil, true, privErr
			}
			return filePathResult{Path: target.path}, true, nil
		}
		result, err := policy.delete(target, req.Recursive)
		return result, false, err
	default:
		return nil, false, fileError(http.StatusBadRequest, "unsupported file action")
	}
}

// writeFileData is the single write path: the unprivileged os.Root write for an
// ordinary root, the privileged helper for a privileged one. It is shared by
// the JSON action and the raw-content route so a privileged root cannot be
// reachable through one of them only.
func (a *App) writeFileData(ctx context.Context, policy *filePolicy, target fileTarget, data []byte) (fileWriteResult, bool, *fileActionError) {
	if target.privileged {
		if err := checkPolicyWrite(policy, target, int64(len(data))); err != nil {
			return fileWriteResult{}, false, err
		}
		if err := a.privWrite(ctx, target, data, privWriteModeFor(target.path)); err != nil {
			return fileWriteResult{}, true, err
		}
		return fileWriteResult{Path: target.path, Size: int64(len(data))}, true, nil
	}
	result, err := policy.write(target, data)
	return result, false, err
}

// errPrivilegedMoveOrCopy is the refusal for a move or copy that touches a
// privileged root. The helper implements write, mkdir and remove; renaming
// inside a root-owned directory needs root just as much as writing there, so
// the operation is reported as unimplemented instead of failing halfway.
var errPrivilegedMoveOrCopy = fileError(http.StatusNotImplemented,
	"move and copy are not supported inside a privileged root; write the new path and delete the old one")

// checkPolicyWrite applies the write gate and the root itself check that the
// unprivileged path also enforces, so a privileged root cannot be a way around
// the read-only switch.
func checkPolicyWrite(policy *filePolicy, target fileTarget, size int64) *fileActionError {
	if !policy.allowWrite {
		return fileError(http.StatusForbidden, "file API is read-only")
	}
	if target.rel == "." {
		return fileError(http.StatusBadRequest, "refusing to modify a configured root")
	}
	if policy.maxWrite > 0 && size > policy.maxWrite {
		return fileError(http.StatusRequestEntityTooLarge, "body exceeds maxWriteBytes (%d)", policy.maxWrite)
	}
	return nil
}
