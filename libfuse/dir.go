// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libfuse

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/keybase/kbfs/libfs"
	"github.com/keybase/kbfs/libkbfs"
	"golang.org/x/net/context"
)

// Folder represents the info shared among all nodes of a KBFS
// top-level folder.
type Folder struct {
	fs   *FS
	list *FolderList

	handleMu sync.RWMutex
	h        *libkbfs.TlfHandle

	folderBranchMu sync.Mutex
	folderBranch   libkbfs.FolderBranch

	// Protects the nodes map.
	nodesMu sync.Mutex
	// Map KBFS nodes to FUSE nodes, to be able to handle multiple
	// lookups and incoming change notifications. A node is present
	// here if the kernel holds a reference to it.
	//
	// If we ever support hardlinks, this would need refcounts.
	//
	// Children must call folder.forgetChildLocked on receiving the
	// FUSE Forget request.
	nodes map[libkbfs.NodeID]fs.Node

	// Protects the updateChan.
	updateMu sync.Mutex
	// updateChan is non-nil when the user disables updates via the
	// file system.  Sending a struct{}{} on this channel will unpause
	// the updates.
	updateChan chan<- struct{}
}

func newFolder(fl *FolderList, h *libkbfs.TlfHandle) *Folder {
	f := &Folder{
		fs:    fl.fs,
		list:  fl,
		h:     h,
		nodes: map[libkbfs.NodeID]fs.Node{},
	}
	return f
}

func (f *Folder) name() libkbfs.CanonicalTlfName {
	f.handleMu.RLock()
	defer f.handleMu.RUnlock()
	return f.h.GetCanonicalName()
}

func (f *Folder) reportErr(ctx context.Context,
	mode libkbfs.ErrorModeType, err error) {
	if err == nil {
		f.fs.errLog.CDebugf(ctx, "Request complete")
		return
	}

	f.fs.config.Reporter().ReportErr(ctx, f.name(), f.list.public, mode, err)
	// We just log the error as debug, rather than error, because it
	// might just indicate an expected error such as an ENOENT.
	//
	// TODO: Classify errors and escalate the logging level of the
	// important ones.
	f.fs.errLog.CDebugf(ctx, err.Error())
}

func (f *Folder) setFolderBranch(folderBranch libkbfs.FolderBranch) error {
	f.folderBranchMu.Lock()
	defer f.folderBranchMu.Unlock()

	// TODO unregister all at unmount
	err := f.list.fs.config.Notifier().RegisterForChanges(
		[]libkbfs.FolderBranch{folderBranch}, f)
	if err != nil {
		return err
	}
	f.folderBranch = folderBranch
	return nil
}

func (f *Folder) unsetFolderBranch(ctx context.Context) {
	f.folderBranchMu.Lock()
	defer f.folderBranchMu.Unlock()
	if f.folderBranch == (libkbfs.FolderBranch{}) {
		// Wasn't set.
		return
	}

	err := f.list.fs.config.Notifier().UnregisterFromChanges([]libkbfs.FolderBranch{f.folderBranch}, f)
	if err != nil {
		f.fs.log.Info("cannot unregister change notifier for folder %q: %v",
			f.name(), err)
	}
	f.folderBranch = libkbfs.FolderBranch{}
}

func (f *Folder) getFolderBranch() libkbfs.FolderBranch {
	f.folderBranchMu.Lock()
	defer f.folderBranchMu.Unlock()
	return f.folderBranch
}

// forgetNode forgets a formerly active child with basename name.
func (f *Folder) forgetNode(node libkbfs.Node) {
	f.nodesMu.Lock()
	defer f.nodesMu.Unlock()

	delete(f.nodes, node.GetID())
	if len(f.nodes) == 0 {
		ctx := libkbfs.BackgroundContextWithCancellationDelayer()
		defer libkbfs.CleanupCancellationDelayer(ctx)
		f.unsetFolderBranch(ctx)
		f.list.forgetFolder(string(f.name()))
	}
}

var _ libkbfs.Observer = (*Folder)(nil)

func (f *Folder) resolve(ctx context.Context) (*libkbfs.TlfHandle, error) {
	// In case there were any unresolved assertions, try them again on
	// the first load.  Otherwise, since we haven't subscribed to
	// updates yet for this folder, we might have missed a name
	// change.
	handle, err := f.h.ResolveAgain(ctx, f.fs.config.KBPKI())
	if err != nil {
		return nil, err
	}
	eq, err := f.h.Equals(f.fs.config.Codec(), *handle)
	if err != nil {
		return nil, err
	}
	if !eq {
		// Make sure the name changes in the folder and the folder list
		f.TlfHandleChange(ctx, handle)
	}
	return handle, nil
}

// invalidateNodeDataRange notifies the kernel to invalidate cached data for node.
//
// The arguments follow KBFS semantics:
//
//     - Len > 0: "bytes Off..Off+Len were mutated"
//     - Len == 0: "new file Len is Off"
//
// For comparison, the FUSE semantics are:
//
//     - Len < 0: "forget data in range Off..infinity"
//     - Len > 0: "forget data in range Off..Off+Len"
func (f *Folder) invalidateNodeDataRange(node fs.Node, write libkbfs.WriteRange) error {
	if file, ok := node.(*File); ok {
		file.eiCache.destroy()
	}
	off := int64(write.Off)
	size := int64(write.Len)
	if write.Off > math.MaxInt64 || write.Len > math.MaxInt64 {
		// out of bounds, just invalidate all data
		off = 0
		size = -1
	}
	if write.Len == 0 {
		// truncate, invalidate all data in the now-lost tail
		size = -1
	}
	// Off=0 Len=0 is the same as calling InvalidateNodeDataAttr; we
	// can just let that go through InvalidateNodeDataRange.
	if err := f.fs.fuse.InvalidateNodeDataRange(node, off, size); err != nil {
		return err
	}
	return nil
}

// LocalChange is called for changes originating within in this process.
func (f *Folder) LocalChange(ctx context.Context, node libkbfs.Node, write libkbfs.WriteRange) {
	if !f.fs.conn.Protocol().HasInvalidate() {
		// OSXFUSE 2.x does not support notifications
		return
	}
	if origin, ok := ctx.Value(libfs.CtxAppIDKey).(*FS); ok && origin == f.fs {
		return
	}

	// Handle in the background because we shouldn't lock during the
	// notification.
	f.fs.queueNotification(func() { f.localChangeInvalidate(ctx, node, write) })
}

func (f *Folder) localChangeInvalidate(ctx context.Context, node libkbfs.Node,
	write libkbfs.WriteRange) {
	f.nodesMu.Lock()
	n, ok := f.nodes[node.GetID()]
	f.nodesMu.Unlock()
	if !ok {
		return
	}

	if err := f.invalidateNodeDataRange(n, write); err != nil && err != fuse.ErrNotCached {
		// TODO we have no mechanism to do anything about this
		f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
	}
}

// BatchChanges is called for changes originating anywhere, including
// other hosts.
func (f *Folder) BatchChanges(ctx context.Context, changes []libkbfs.NodeChange) {
	if !f.fs.conn.Protocol().HasInvalidate() {
		// OSXFUSE 2.x does not support notifications
		return
	}
	if origin, ok := ctx.Value(libfs.CtxAppIDKey).(*FS); ok && origin == f.fs {
		return
	}
	if v := ctx.Value(libkbfs.CtxBackgroundSyncKey); v != nil {
		return
	}

	// Handle in the background because we shouldn't lock during the
	// notification.
	f.fs.queueNotification(func() { f.batchChangesInvalidate(ctx, changes) })
}

func (f *Folder) batchChangesInvalidate(ctx context.Context,
	changes []libkbfs.NodeChange) {
	for _, v := range changes {
		f.nodesMu.Lock()
		n, ok := f.nodes[v.Node.GetID()]
		f.nodesMu.Unlock()
		if !ok {
			continue
		}

		switch {
		case len(v.DirUpdated) > 0:
			// invalidate potentially cached Readdir contents
			if err := f.fs.fuse.InvalidateNodeData(n); err != nil && err != fuse.ErrNotCached {
				// TODO we have no mechanism to do anything about this
				f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
			}
			for _, name := range v.DirUpdated {
				// invalidate the dentry cache
				if err := f.fs.fuse.InvalidateEntry(n, name); err != nil && err != fuse.ErrNotCached {
					// TODO we have no mechanism to do anything about this
					f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
				}
			}

		case len(v.FileUpdated) > 0:
			for _, write := range v.FileUpdated {
				if err := f.invalidateNodeDataRange(n, write); err != nil && err != fuse.ErrNotCached {
					// TODO we have no mechanism to do anything about this
					f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
				}
			}

		default:
			if file, ok := n.(*File); ok {
				file.eiCache.destroy()
			}
			// just the attributes
			if err := f.fs.fuse.InvalidateNodeAttr(n); err != nil && err != fuse.ErrNotCached {
				// TODO we have no mechanism to do anything about this
				f.fs.log.CErrorf(ctx, "FUSE invalidate error: %v", err)
			}
		}
	}
}

// TlfHandleChange is called when the name of a folder changes.
func (f *Folder) TlfHandleChange(ctx context.Context,
	newHandle *libkbfs.TlfHandle) {
	f.fs.log.CDebugf(ctx, "TlfHandleChange called %v", newHandle.GetCanonicalName())
	// Handle in the background because we shouldn't lock during the
	// notification
	f.fs.queueNotification(func() {
		f.tlfHandleChangeInvalidate(ctx, newHandle)
	})
}

func (f *Folder) tlfHandleChangeInvalidate(ctx context.Context,
	newHandle *libkbfs.TlfHandle) {
	oldName := func() libkbfs.CanonicalTlfName {
		f.handleMu.Lock()
		defer f.handleMu.Unlock()
		oldName := f.h.GetCanonicalName()
		f.h = newHandle
		return oldName
	}()

	f.list.updateTlfName(ctx, string(oldName),
		string(newHandle.GetCanonicalName()))
}

// TODO: Expire TLF nodes periodically. See
// https://keybase.atlassian.net/browse/KBFS-59 .

// DirInterface gathers all the interfaces a Dir or something that
// wraps a Dir should implement.
type DirInterface interface {
	fs.Node
	fs.NodeRequestLookuper
	fs.NodeCreater
	fs.NodeMkdirer
	fs.NodeSymlinker
	fs.NodeRenamer
	fs.NodeRemover
	fs.Handle
	fs.HandleReadDirAller
	fs.NodeForgetter
	fs.NodeSetattrer
}

// Dir represents a subdirectory of a KBFS top-level folder (including
// the TLF root directory itself).
type Dir struct {
	folder *Folder
	node   libkbfs.Node
}

func newDir(folder *Folder, node libkbfs.Node) *Dir {
	d := &Dir{
		folder: folder,
		node:   node,
	}
	return d
}

var _ DirInterface = (*Dir)(nil)

// Attr implements the fs.Node interface for Dir.
func (d *Dir) Attr(ctx context.Context, a *fuse.Attr) (err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir Attr")
	defer func() { d.folder.reportErr(ctx, libkbfs.ReadMode, err) }()

	// This fits in situation 1 as described in libkbfs/delayed_cancellation.go
	err = libkbfs.EnableDelayedCancellationWithGracePeriod(
		ctx, d.folder.fs.config.DelayedCancellationGracePeriod())
	if err != nil {
		return err
	}

	return d.attr(ctx, a)
}

func (d *Dir) attr(ctx context.Context, a *fuse.Attr) (err error) {
	de, err := d.folder.fs.config.KBFSOps().Stat(ctx, d.node)
	if err != nil {
		if isNoSuchNameError(err) {
			return fuse.ESTALE
		}
		return err
	}
	fillAttr(&de, a)

	a.Mode = os.ModeDir | 0700
	if d.folder.list.public {
		a.Mode |= 0055
	}
	return nil
}

// Lookup implements the fs.NodeRequestLookuper interface for Dir.
func (d *Dir) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (node fs.Node, err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir Lookup %s", req.Name)
	defer func() { d.folder.reportErr(ctx, libkbfs.ReadMode, err) }()

	// This fits in situation 1 as described in libkbfs/delayed_cancellation.go
	err = libkbfs.EnableDelayedCancellationWithGracePeriod(
		ctx, d.folder.fs.config.DelayedCancellationGracePeriod())
	if err != nil {
		return nil, err
	}

	specialNode := handleTLFSpecialFile(
		req.Name, d.folder, &resp.EntryValid)
	if specialNode != nil {
		return specialNode, nil
	}

	// Check if this is a per-file metainformation file, if so
	// return the corresponding SpecialReadFile.
	if strings.HasPrefix(req.Name, libfs.FileInfoPrefix) {
		node, _, err := d.folder.fs.config.KBFSOps().Lookup(ctx, d.node, req.Name[len(libfs.FileInfoPrefix):])
		if err != nil {
			return nil, err
		}
		nmd, err := d.folder.fs.config.KBFSOps().GetNodeMetadata(ctx, node)
		if err != nil {
			return nil, err
		}
		return &SpecialReadFile{fileInfo(nmd).read}, nil
	}

	newNode, de, err := d.folder.fs.config.KBFSOps().Lookup(ctx, d.node, req.Name)
	if err != nil {
		if _, ok := err.(libkbfs.NoSuchNameError); ok {
			return nil, fuse.ENOENT
		}
		return nil, err
	}

	// No libkbfs calls after this point!
	d.folder.nodesMu.Lock()
	defer d.folder.nodesMu.Unlock()

	// newNode can be nil even without errors when the KBFS direntry
	// is of a type that doesn't get its own node (is fully contained
	// in the directory); Symlink does this.
	if newNode != nil {
		if n, ok := d.folder.nodes[newNode.GetID()]; ok {
			return n, nil
		}
	}

	switch de.Type {
	default:
		return nil, fmt.Errorf("unhandled entry type: %v", de.Type)

	case libkbfs.File, libkbfs.Exec:
		child := &File{
			folder: d.folder,
			node:   newNode,
		}
		d.folder.nodes[newNode.GetID()] = child
		return child, nil

	case libkbfs.Dir:
		child := newDir(d.folder, newNode)
		d.folder.nodes[newNode.GetID()] = child
		return child, nil

	case libkbfs.Sym:
		child := &Symlink{
			parent: d,
			name:   req.Name,
		}
		// a Symlink is never included in Folder.nodes, as it doesn't
		// have a libkbfs.Node to keep track of renames.
		return child, nil
	}
}

type fileInfo libkbfs.NodeMetadata

func (fi fileInfo) read(ctx context.Context) ([]byte, time.Time, error) {
	bs, err := libfs.PrettyJSON(fi)
	return bs, time.Time{}, err
}

func getEXCLFromCreateRequest(req *fuse.CreateRequest) libkbfs.Excl {
	return libkbfs.Excl(req.Flags&fuse.OpenExclusive == fuse.OpenExclusive)
}

// Create implements the fs.NodeCreater interface for Dir.
func (d *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (node fs.Node, handle fs.Handle, err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir Create %s", req.Name)
	defer func() { d.folder.reportErr(ctx, libkbfs.WriteMode, err) }()

	isExec := (req.Mode.Perm() & 0100) != 0
	excl := getEXCLFromCreateRequest(req)
	newNode, ei, err := d.folder.fs.config.KBFSOps().CreateFile(
		ctx, d.node, req.Name, isExec, excl)
	if err != nil {
		return nil, nil, err
	}

	child := &File{
		folder: d.folder,
		node:   newNode,
	}

	// Create is normally followed an Attr call. Fuse uses the same context for
	// them. If the context is cancelled after the Create call enters the
	// critical portion, and grace period has passed before Attr happens, the
	// Attr can result in EINTR which application does not expect. This caches
	// the EntryInfo for the created node and allows the subsequent Attr call to
	// use the cached EntryInfo instead of relying on a new Stat call.
	if reqID, ok := ctx.Value(CtxIDKey).(string); ok {
		child.eiCache.set(reqID, ei)
	}

	d.folder.nodesMu.Lock()
	d.folder.nodes[newNode.GetID()] = child
	d.folder.nodesMu.Unlock()
	return child, child, nil
}

// Mkdir implements the fs.NodeMkdirer interface for Dir.
func (d *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (
	node fs.Node, err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir Mkdir %s", req.Name)
	defer func() { d.folder.reportErr(ctx, libkbfs.WriteMode, err) }()

	// This fits in situation 1 as described in libkbfs/delayed_cancellation.go
	err = libkbfs.EnableDelayedCancellationWithGracePeriod(
		ctx, d.folder.fs.config.DelayedCancellationGracePeriod())
	if err != nil {
		return nil, err
	}

	newNode, _, err := d.folder.fs.config.KBFSOps().CreateDir(
		ctx, d.node, req.Name)
	if err != nil {
		return nil, err
	}

	child := newDir(d.folder, newNode)
	d.folder.nodesMu.Lock()
	d.folder.nodes[newNode.GetID()] = child
	d.folder.nodesMu.Unlock()
	return child, nil
}

// Symlink implements the fs.NodeSymlinker interface for Dir.
func (d *Dir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (
	node fs.Node, err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir Symlink %s -> %s",
		req.NewName, req.Target)
	defer func() { d.folder.reportErr(ctx, libkbfs.WriteMode, err) }()

	// This fits in situation 1 as described in libkbfs/delayed_cancellation.go
	err = libkbfs.EnableDelayedCancellationWithGracePeriod(
		ctx, d.folder.fs.config.DelayedCancellationGracePeriod())
	if err != nil {
		return nil, err
	}

	if _, err := d.folder.fs.config.KBFSOps().CreateLink(
		ctx, d.node, req.NewName, req.Target); err != nil {
		return nil, err
	}

	child := &Symlink{
		parent: d,
		name:   req.NewName,
	}
	return child, nil
}

// Rename implements the fs.NodeRenamer interface for Dir.
func (d *Dir) Rename(ctx context.Context, req *fuse.RenameRequest,
	newDir fs.Node) (err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir Rename %s -> %s",
		req.OldName, req.NewName)
	defer func() { d.folder.reportErr(ctx, libkbfs.WriteMode, err) }()

	var realNewDir *Dir
	switch newDir := newDir.(type) {
	case *Dir:
		realNewDir = newDir
	case *TLF:
		var err error
		realNewDir, err = newDir.loadDir(ctx)
		if err != nil {
			return err
		}
	default:
		// The destination is not a TLF instance, probably
		// because it's Root (or some other node type added
		// later). The kernel won't let a rename newDir point
		// to a non-directory.
		//
		// We have no cheap atomic rename across folders, so
		// we can't serve this. EXDEV makes `mv` do a
		// copy+delete, and the Lookup on the destination path
		// will decide whether it's legal.
		return fuse.Errno(syscall.EXDEV)
	}

	if d.folder != realNewDir.folder {
		// Check this explicitly, not just trusting KBFSOps.Rename to
		// return an error, because we rely on it for locking
		// correctness.
		return fuse.Errno(syscall.EXDEV)
	}

	// overwritten node, if any, will be removed from Folder.nodes, if
	// it is there in the first place, by its Forget

	if err := d.folder.fs.config.KBFSOps().Rename(
		ctx, d.node, req.OldName, realNewDir.node, req.NewName); err != nil {
		return err
	}

	return nil
}

// Remove implements the fs.NodeRemover interface for Dir.
func (d *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) (err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir Remove %s", req.Name)
	defer func() { d.folder.reportErr(ctx, libkbfs.WriteMode, err) }()

	// This fits in situation 1 as described in libkbfs/delayed_cancellation.go
	err = libkbfs.EnableDelayedCancellationWithGracePeriod(
		ctx, d.folder.fs.config.DelayedCancellationGracePeriod())
	if err != nil {
		return err
	}

	// node will be removed from Folder.nodes, if it is there in the
	// first place, by its Forget

	if req.Dir {
		err = d.folder.fs.config.KBFSOps().RemoveDir(ctx, d.node, req.Name)
	} else {
		err = d.folder.fs.config.KBFSOps().RemoveEntry(ctx, d.node, req.Name)
	}
	if err != nil {
		return err
	}

	return nil
}

// ReadDirAll implements the fs.NodeReadDirAller interface for Dir.
func (d *Dir) ReadDirAll(ctx context.Context) (res []fuse.Dirent, err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir ReadDirAll")
	defer func() { d.folder.reportErr(ctx, libkbfs.ReadMode, err) }()

	children, err := d.folder.fs.config.KBFSOps().GetDirChildren(ctx, d.node)
	if err != nil {
		return nil, err
	}

	for name, ei := range children {
		fde := fuse.Dirent{
			Name: name,
		}
		switch ei.Type {
		case libkbfs.File, libkbfs.Exec:
			fde.Type = fuse.DT_File
		case libkbfs.Dir:
			fde.Type = fuse.DT_Dir
		case libkbfs.Sym:
			fde.Type = fuse.DT_Link
		}
		res = append(res, fde)
	}
	return res, nil
}

// Forget kernel reference to this node.
func (d *Dir) Forget() {
	d.folder.forgetNode(d.node)
}

// Setattr implements the fs.NodeSetattrer interface for Dir.
func (d *Dir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) (err error) {
	d.folder.fs.log.CDebugf(ctx, "Dir SetAttr")
	defer func() { d.folder.reportErr(ctx, libkbfs.WriteMode, err) }()

	valid := req.Valid

	if valid.Mode() {
		// You can't set the mode on KBFS directories, but we don't
		// want to return EPERM because that unnecessarily fails some
		// applications like unzip.  Instead ignore it, print a debug
		// message, and advertise this behavior on the
		// "understand_kbfs" doc online.
		d.folder.fs.log.CDebugf(ctx, "Ignoring unsupported attempt to set "+
			"the mode on a directory")
		valid &^= fuse.SetattrMode
	}

	if valid.Mtime() {
		err := d.folder.fs.config.KBFSOps().SetMtime(
			ctx, d.node, &req.Mtime)
		if err != nil {
			return err
		}
		valid &^= fuse.SetattrMtime | fuse.SetattrMtimeNow
	}

	// KBFS has no concept of persistent atime; explicitly don't handle it
	valid &^= fuse.SetattrAtime | fuse.SetattrAtimeNow

	// things we don't need to explicitly handle
	valid &^= fuse.SetattrLockOwner | fuse.SetattrHandle

	if valid.Uid() || valid.Gid() {
		// You can't set the UID/GID on KBFS directories, but we don't
		// want to return ENOSYS because that causes scary warnings on
		// some programs like mv.  Instead ignore it, print a debug
		// message, and advertise this behavior on the
		// "understand_kbfs" doc online.
		d.folder.fs.log.CDebugf(ctx, "Ignoring unsupported attempt to set "+
			"the UID/GID on a directory")
		valid &^= fuse.SetattrUid | fuse.SetattrGid
	}

	if valid != 0 {
		// don't let an unhandled operation slip by without error
		d.folder.fs.log.CInfof(ctx, "Setattr did not handle %v", valid)
		return fuse.ENOSYS
	}

	// Something in Linux kernel *requires* directories to provide
	// attributes here, where it was just an optimization for files.
	if err := d.attr(ctx, &resp.Attr); err != nil {
		return err
	}
	return nil
}

// isNoSuchNameError checks for libkbfs.NoSuchNameError.
func isNoSuchNameError(err error) bool {
	_, ok := err.(libkbfs.NoSuchNameError)
	return ok
}
