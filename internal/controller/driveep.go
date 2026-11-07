package controller

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/model"
)

// driveEP is the endpoint of a connected USB drive. Paths on it are relative
// to the drive's root ("/" is the root); they are mapped to the mount point
// and passed to the controller's local endpoint.
//
// Every call first checks that the drive is still mounted there (the mount
// guard): an unplugged drive leaves an empty folder behind, and writing into
// it would fill the Unraid root file system, which is RAM. A failed check
// and I/O errors that mean the device is gone wrap model.ErrOffline, which
// stops a run at once, without retries.
type driveEP struct {
	local   *endpoint.Local
	mount   string // absolute mount point on the controller
	name    string // for messages
	version string
	mounted func(string) bool
}

var (
	_ endpoint.Endpoint    = (*driveEP)(nil)
	_ endpoint.Resumer     = (*driveEP)(nil)
	_ endpoint.Mkdirer     = (*driveEP)(nil)
	_ endpoint.TrashLister = (*driveEP)(nil)
)

func (d *driveEP) gone(detail string) error {
	return fmt.Errorf("USB drive %s removed or failing: %s (%w)", d.name, detail, model.ErrOffline)
}

// check is the mount guard.
func (d *driveEP) check() error {
	if !d.mounted(d.mount) {
		return d.gone("no longer mounted at " + d.mount)
	}
	return nil
}

// abs maps a path on the drive to the controller's path.
func (d *driveEP) abs(p string) (string, error) {
	c, err := model.CleanAbs(p)
	if err != nil {
		return "", err
	}
	if c == "/" {
		return d.mount, nil
	}
	return d.mount + c, nil
}

// wrap turns errors that mean the device is gone into ErrOffline.
func (d *driveEP) wrap(err error) error {
	if err == nil {
		return nil
	}
	for _, e := range []error{syscall.EIO, syscall.ENODEV, syscall.ENXIO, syscall.ESTALE} {
		if errors.Is(err, e) {
			return d.gone(err.Error())
		}
	}
	if !d.mounted(d.mount) {
		return d.gone("no longer mounted at " + d.mount)
	}
	return err
}

// prep checks the mount and maps p.
func (d *driveEP) prep(p string) (string, error) {
	if err := d.check(); err != nil {
		return "", err
	}
	return d.abs(p)
}

func (d *driveEP) Info(ctx context.Context) (model.NodeInfo, error) {
	if err := d.check(); err != nil {
		return model.NodeInfo{}, err
	}
	info := model.NodeInfo{Hostname: d.name, Version: d.version}
	u, err := d.local.StatFS(ctx, d.mount)
	if err != nil {
		return info, d.wrap(err)
	}
	info.Roots = []model.RootInfo{{Path: "/", Total: u.Total, Free: u.Free}}
	info.Disks = []model.DiskInfo{{Path: "/", Total: u.Total, Used: u.Total - u.Free, Free: u.Free}}
	return info, nil
}

func (d *driveEP) ListDir(ctx context.Context, path string) ([]model.Entry, error) {
	p, err := d.prep(path)
	if err != nil {
		return nil, err
	}
	out, err := d.local.ListDir(ctx, p)
	if err == nil && p == d.mount {
		// The marker is BFBackup's, not backup data.
		keep := out[:0]
		for _, e := range out {
			if e.Name != model.DriveMarkerName {
				keep = append(keep, e)
			}
		}
		out = keep
	}
	return out, d.wrap(err)
}

func (d *driveEP) Scan(ctx context.Context, req model.ScanRequest, fn func(model.DirListing) error) error {
	p, err := d.prep(req.Root)
	if err != nil {
		return err
	}
	req.Root = p
	return d.wrap(d.local.Scan(ctx, req, fn))
}

func (d *driveEP) Open(ctx context.Context, path string) (endpoint.Reader, error) {
	p, err := d.prep(path)
	if err != nil {
		return nil, err
	}
	r, err := d.local.Open(ctx, p)
	return r, d.wrap(err)
}

func (d *driveEP) OpenAt(ctx context.Context, path string, offset int64) (endpoint.Reader, error) {
	p, err := d.prep(path)
	if err != nil {
		return nil, err
	}
	r, err := d.local.OpenAt(ctx, p, offset)
	return r, d.wrap(err)
}

func (d *driveEP) Create(ctx context.Context, path string, e model.Entry) (endpoint.Writer, error) {
	p, err := d.prep(path)
	if err != nil {
		return nil, err
	}
	w, err := d.local.Create(ctx, p, e)
	if err != nil {
		return nil, d.wrap(err)
	}
	return driveWriter{w, d}, nil
}

func (d *driveEP) CreateResumable(ctx context.Context, path string, e model.Entry) (endpoint.Writer, int64, error) {
	p, err := d.prep(path)
	if err != nil {
		return nil, 0, err
	}
	w, have, err := d.local.CreateResumable(ctx, p, e)
	if err != nil {
		return nil, 0, d.wrap(err)
	}
	return driveWriter{w, d}, have, nil
}

// driveWriter checks the mount again before a file is moved into place.
type driveWriter struct {
	endpoint.Writer
	d *driveEP
}

func (w driveWriter) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	return n, w.d.wrap(err)
}

func (w driveWriter) Commit(ctx context.Context, expect string) (string, error) {
	if err := w.d.check(); err != nil {
		w.Writer.Abort()
		return "", err
	}
	sum, err := w.Writer.Commit(ctx, expect)
	return sum, w.d.wrap(err)
}

func (d *driveEP) Symlink(ctx context.Context, path, target string) error {
	p, err := d.prep(path)
	if err != nil {
		return err
	}
	return d.wrap(d.local.Symlink(ctx, p, target))
}

func (d *driveEP) Stat(ctx context.Context, paths []string) ([]model.StatResult, error) {
	if err := d.check(); err != nil {
		return nil, err
	}
	mapped := make([]string, len(paths))
	for i, p := range paths {
		a, err := d.abs(p)
		if err != nil {
			return nil, err
		}
		mapped[i] = a
	}
	out, err := d.local.Stat(ctx, mapped)
	if err != nil {
		return nil, d.wrap(err)
	}
	for i := range out {
		out[i].Path = paths[i]
	}
	return out, nil
}

func (d *driveEP) Hash(ctx context.Context, path string) (string, error) {
	p, err := d.prep(path)
	if err != nil {
		return "", err
	}
	h, err := d.local.Hash(ctx, p)
	return h, d.wrap(err)
}

func (d *driveEP) Trash(ctx context.Context, root, rel, batch string) error {
	p, err := d.prep(root)
	if err != nil {
		return err
	}
	return d.wrap(d.local.Trash(ctx, p, rel, batch))
}

func (d *driveEP) TrashInfo(ctx context.Context, root string) ([]model.TrashBatch, error) {
	p, err := d.prep(root)
	if err != nil {
		return nil, err
	}
	out, err := d.local.TrashInfo(ctx, p)
	return out, d.wrap(err)
}

func (d *driveEP) Remove(ctx context.Context, root, rel string) error {
	p, err := d.prep(root)
	if err != nil {
		return err
	}
	return d.wrap(d.local.Remove(ctx, p, rel))
}

func (d *driveEP) PurgeTrash(ctx context.Context, root string, olderThan time.Time) (int, error) {
	p, err := d.prep(root)
	if err != nil {
		return 0, err
	}
	n, err := d.local.PurgeTrash(ctx, p, olderThan)
	return n, d.wrap(err)
}

func (d *driveEP) StatFS(ctx context.Context, path string) (model.FSUsage, error) {
	p, err := d.prep(path)
	if err != nil {
		return model.FSUsage{}, err
	}
	u, err := d.local.StatFS(ctx, p)
	return u, d.wrap(err)
}

func (d *driveEP) Mkdir(ctx context.Context, path string) error {
	p, err := d.prep(path)
	if err != nil {
		return err
	}
	return d.wrap(d.local.Mkdir(ctx, p))
}

// RunHook: hooks run on the source node; a drive has none.
func (d *driveEP) RunHook(ctx context.Context, name string, env map[string]string) (model.HookResult, error) {
	return model.HookResult{}, fmt.Errorf("USB drive %s: hooks run on the source node: %w", d.name, model.ErrUnsupported)
}
