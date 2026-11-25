package klipper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
	"github.com/prusa3d/prusa-connect/sdk/fault"
)

// The gcodes folder of one machine, exposed to the host as one storage.
//
// A host path is storage-prefixed and relative: "usb/parts/bracket.gcode" is
// "parts/bracket.gcode" in Moonraker's gcodes root. Listing, folders and
// deleting go over the websocket. Uploads and thumbnails go over HTTP.

const (
	// fileChangeBacklog bounds the file changes that wait for the host.
	fileChangeBacklog = 64

	// maxPreviewBytes bounds one thumbnail. A slicer thumbnail is a few tens
	// of kilobytes.
	maxPreviewBytes = 1 << 20

	// maxErrorBody bounds what is read of an HTTP refusal.
	maxErrorBody = 16 << 10

	// progressStep is how many bytes pass between two progress reports.
	progressStep = 256 << 10

	// storageType is the kind of storage a page shows.
	storageType = "LOCAL"
)

// files is the gcodes root of one machine.
type files struct {
	session *Session
	base    string
	apiKey  string
	client  *http.Client
	storage string

	mu    sync.Mutex
	known bool
	usage *diskUsage
	write bool

	changes chan connectorapi.FileChange
	sending atomic.Bool
}

var _ interface {
	FilesChanged() <-chan connectorapi.FileChange
} = (*Session)(nil)

// newFiles builds the gcodes root of one session.
func newFiles(s *Session, base, apiKey string, client *http.Client, storage string) *files {
	return &files{
		session: s,
		base:    base,
		apiKey:  apiKey,
		client:  client,
		storage: storage,
		write:   true,
		changes: make(chan connectorapi.FileChange, fileChangeBacklog),
	}
}

// relative turns a host path into a path under the gcodes root. A leading
// separator is accepted. The storage segment must be this machine's.
func (f *files) relative(hostPath string) (string, error) {
	if refusal := connectorapi.RefusePathEscape(hostPath); refusal != nil {
		return "", refusal
	}

	trimmed := strings.Trim(hostPath, "/")
	first, rest, _ := strings.Cut(trimmed, "/")

	if first != f.storage {
		return "", connectorapi.PathNotAllowed.New("the path names a storage this printer does not have").
			With(fault.StringParam("path", hostPath))
	}

	return rest, nil
}

// hostPath is the host's spelling of a path under the gcodes root.
func (f *files) hostPath(rel string) string {
	if rel == "" {
		return f.storage
	}

	return f.storage + "/" + strings.Trim(rel, "/")
}

// rootPath is Moonraker's spelling of a path under the gcodes root.
func rootPath(rel string) string {
	if rel == "" {
		return gcodesRoot
	}

	return gcodesRoot + "/" + rel
}

// remember keeps what a listing of the root said about the disk.
func (f *files) remember(reply directoryReply) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.known = true

	if reply.DiskUsage != nil {
		usage := *reply.DiskUsage
		f.usage = &usage
	}

	if reply.RootInfo != nil && reply.RootInfo.Permissions != "" {
		f.write = strings.Contains(reply.RootInfo.Permissions, "w")
	}
}

// Storages is the one storage this machine has, as last listed.
func (s *Session) Storages(context.Context) ([]connectorapi.Storage, error) {
	f := s.files

	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.known {
		return []connectorapi.Storage{}, nil
	}

	storage := connectorapi.Storage{
		Mountpoint: "/" + f.storage,
		Name:       f.storage,
		Type:       storageType,
		ReadOnly:   !f.write,
	}

	if f.usage != nil {
		storage.FreeSpace, storage.FreeSpaceKnown = f.usage.Free, true
	}

	return []connectorapi.Storage{storage}, nil
}

// probeRoot lists the gcodes root once during the handshake, so Storages has
// an answer before anybody asks. A Moonraker with no gcodes root has no storage.
func (s *Session) probeRoot(ctx context.Context) {
	var reply directoryReply
	if err := s.callSync(ctx, methodGetDirectory, map[string]any{"path": gcodesRoot}, &reply); err != nil {
		s.log.Warn("the machine has no gcodes folder, so it offers no files", "error", err.Error())

		return
	}

	s.files.remember(reply)
}

// List returns one directory, or one file with its preview and slicer data.
func (s *Session) List(ctx context.Context, hostPath string) (connectorapi.Listing, error) {
	if refusal := connectorapi.RefusePathEscape(hostPath); refusal != nil {
		return connectorapi.Listing{}, refusal
	}

	f := s.files

	if strings.Trim(hostPath, "/") == "" {
		return f.rootListing(), nil
	}

	rel, err := f.relative(hostPath)
	if err != nil {
		return connectorapi.Listing{}, err
	}

	var reply directoryReply

	refusal, err := s.callInto(ctx, methodGetDirectory, map[string]any{"path": rootPath(rel)}, &reply)
	if err != nil {
		return connectorapi.Listing{}, err
	}

	if refusal == nil {
		if rel == "" {
			f.remember(reply)
		}

		return f.directoryListing(rel, reply), nil
	}

	// Not a folder. It may be a file, which is listed with its metadata.
	file, fileRefusal, err := s.fileInfo(ctx, rel)
	if err != nil {
		return connectorapi.Listing{}, err
	}

	if fileRefusal != nil {
		return connectorapi.Listing{}, connectorapi.ListingRefused.New(refusal.Message).
			With(fault.StringParam("path", hostPath),
				fault.StringParam("machine_reason", refusal.Message),
				fault.StringParam("reason", refusal.Message)).
			Wrap(connectorapi.ErrListingRefused)
	}

	return connectorapi.Listing{Directory: file}, nil
}

// rootListing is the one storage as a directory entry.
func (f *files) rootListing() connectorapi.Listing {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.known {
		return connectorapi.Listing{Directory: connectorapi.File{Folder: true}}
	}

	return connectorapi.Listing{
		Directory: connectorapi.File{Folder: true},
		Entries: []connectorapi.File{{
			Path: f.storage, Name: f.storage, DisplayName: f.storage, Folder: true, ReadOnly: !f.write,
		}},
	}
}

// directoryListing is one folder of the gcodes root. Hidden entries, such as
// the .thumbs folder Moonraker keeps previews in, are left out.
func (f *files) directoryListing(rel string, reply directoryReply) connectorapi.Listing {
	listing := connectorapi.Listing{Directory: connectorapi.File{
		Path: f.hostPath(rel), Name: path.Base("/" + f.hostPath(rel)), Folder: true,
	}}

	for _, dir := range reply.Dirs {
		if hidden(dir.Dirname) {
			continue
		}

		listing.Entries = append(listing.Entries, connectorapi.File{
			Path:     f.hostPath(path.Join(rel, dir.Dirname)),
			Name:     dir.Dirname,
			Folder:   true,
			Modified: int64(dir.Modified),
			ReadOnly: readOnly(dir.Permissions),
		})
	}

	for _, file := range reply.Files {
		if hidden(file.Filename) {
			continue
		}

		listing.Entries = append(listing.Entries, connectorapi.File{
			Path:     f.hostPath(path.Join(rel, file.Filename)),
			Name:     file.Filename,
			Size:     file.Size,
			Modified: int64(file.Modified),
			ReadOnly: readOnly(file.Permissions),
		})
	}

	return listing
}

// hidden reports a name that starts with a dot.
func hidden(name string) bool { return strings.HasPrefix(name, ".") }

// readOnly reads Moonraker's permissions word. An empty word is writable.
func readOnly(permissions string) bool {
	return permissions != "" && !strings.Contains(permissions, "w")
}

// fileInfo is one file with what the slicer wrote about it.
func (s *Session) fileInfo(ctx context.Context, rel string) (connectorapi.File, *rpcError, error) {
	var meta fileMetadata

	refusal, err := s.callInto(ctx, methodFileMetadata, map[string]any{"filename": rel}, &meta)
	if err != nil || refusal != nil {
		return connectorapi.File{}, refusal, err
	}

	file := connectorapi.File{
		Path:     s.files.hostPath(rel),
		Name:     path.Base(rel),
		Size:     meta.Size,
		Modified: int64(meta.Modified),
		Material: meta.FilamentType,
	}

	if meta.EstimatedTime != nil && *meta.EstimatedTime > 0 {
		file.EstimatedSeconds = int64(*meta.EstimatedTime)
	}

	if meta.LayerHeight != nil {
		file.LayerHeightMM = *meta.LayerHeight
	}

	if thumb, ok := largestThumbnail(meta.Thumbnails); ok {
		preview, previewErr := s.files.fetchThumbnail(ctx, rel, thumb)
		if previewErr != nil {
			s.log.Debug("a file's preview could not be read", "path", rel, "error", previewErr.Error())
		} else {
			file.Preview = preview
		}
	}

	return file, nil, nil
}

// largestThumbnail is the widest thumbnail that fits the preview bound.
func largestThumbnail(thumbs []thumbnail) (thumbnail, bool) {
	var best thumbnail

	found := false

	for _, thumb := range thumbs {
		if thumb.RelativePath == "" || thumb.Size > maxPreviewBytes {
			continue
		}

		if !found || thumb.Width > best.Width {
			best, found = thumb, true
		}
	}

	return best, found
}

// fetchThumbnail reads one thumbnail over HTTP. Its relative path is relative
// to the folder that holds the print file.
func (f *files) fetchThumbnail(ctx context.Context, rel string, thumb thumbnail) ([]byte, error) {
	target := path.Join(path.Dir(rel), thumb.RelativePath)
	if strings.HasPrefix(target, "..") {
		return nil, errors.New("klipper: the thumbnail path leaves the gcodes folder")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.fileURL(target), nil)
	if err != nil {
		return nil, fmt.Errorf("klipper: build the thumbnail request: %w", err)
	}

	f.authorize(request)

	response, err := f.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("klipper: read the thumbnail: %w", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("klipper: the thumbnail answered %d", response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxPreviewBytes+1))
	if err != nil {
		return nil, fmt.Errorf("klipper: read the thumbnail: %w", err)
	}

	if len(body) > maxPreviewBytes {
		return nil, errors.New("klipper: the thumbnail is larger than this driver reads")
	}

	return body, nil
}

// fileURL is the HTTP address of one file under the gcodes root, with each
// segment escaped.
func (f *files) fileURL(rel string) string {
	segments := strings.Split(rel, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}

	return f.base + "/server/files/" + gcodesRoot + "/" + strings.Join(segments, "/")
}

// authorize adds the API key to a request, when there is one.
func (f *files) authorize(request *http.Request) {
	if f.apiKey != "" {
		request.Header.Set(apiKeyHeader, f.apiKey)
	}
}

// CreateFolder makes one folder.
func (s *Session) CreateFolder(ctx context.Context, hostPath string) error {
	return s.changeFiles(ctx, hostPath, methodPostDirectory, false)
}

// DeleteFile removes one file. Moonraker refuses a file that is printing.
func (s *Session) DeleteFile(ctx context.Context, hostPath string) error {
	return s.changeFiles(ctx, hostPath, methodDeleteFile, false)
}

// DeleteFolder removes one folder and everything in it, as a Prusa printer
// does.
func (s *Session) DeleteFolder(ctx context.Context, hostPath string) error {
	return s.changeFiles(ctx, hostPath, methodDeleteDirectory, true)
}

// changeFiles sends one change to the gcodes root.
func (s *Session) changeFiles(ctx context.Context, hostPath, method string, force bool) error {
	rel, err := s.files.relative(hostPath)
	if err != nil {
		return err
	}

	if rel == "" {
		return connectorapi.FileChangeRefused.New("the storage itself cannot be changed").
			With(fault.StringParam("path", hostPath)).
			Wrap(connectorapi.ErrFileChangeRefused)
	}

	params := map[string]any{"path": rootPath(rel)}
	if force {
		params["force"] = true
	}

	refusal, err := s.callInto(ctx, method, params, nil)
	if err != nil {
		return err
	}

	if refusal != nil {
		return connectorapi.FileChangeRefused.New("the printer refused that change to its storage").
			With(fault.StringParam("path", hostPath),
				fault.StringParam("machine_reason", refusal.Message),
				fault.StringParam("reason", refusal.Message)).
			Wrap(connectorapi.ErrFileChangeRefused)
	}

	return nil
}

// FilesChanged carries each file change Moonraker reports, from any client.
func (s *Session) FilesChanged() <-chan connectorapi.FileChange { return s.files.changes }

// changed reads one notify_filelist_changed.
func (f *files) changed(params json.RawMessage) {
	var carried []struct {
		filelistChange
		Source *filelistItem `json:"source_item"`
	}

	if json.Unmarshal(params, &carried) != nil {
		f.session.refused.Add(1)

		return
	}

	for _, change := range carried {
		if change.Item.Root != gcodesRoot {
			continue
		}

		rel := strings.Trim(change.Item.Path, "/")
		if hidden(path.Base("/"+rel)) || strings.Contains(rel, "/.") {
			continue
		}

		entry := connectorapi.File{
			Path: f.hostPath(rel), Name: path.Base("/" + rel), Size: change.Item.Size,
			Modified: int64(change.Item.Modified),
		}

		switch change.Action {
		case "create_file", "modify_file", "upload_file":
			f.emit(connectorapi.FileChange{File: entry})
		case "create_dir":
			entry.Folder = true
			f.emit(connectorapi.FileChange{File: entry})
		case "delete_file", "delete_dir":
			entry.Folder = change.Action == "delete_dir"
			f.emit(connectorapi.FileChange{File: entry, Removed: true})
		case "move_file", "move_dir":
			if change.Source != nil && change.Source.Root == gcodesRoot {
				from := strings.Trim(change.Source.Path, "/")
				f.emit(connectorapi.FileChange{File: connectorapi.File{
					Path: f.hostPath(from), Name: path.Base("/" + from), Folder: change.Action == "move_dir",
				}, Removed: true})
			}

			entry.Folder = change.Action == "move_dir"
			f.emit(connectorapi.FileChange{File: entry})
		case "root_update":
			f.emit(connectorapi.FileChange{
				File: connectorapi.File{Path: f.storage, Name: f.storage, Folder: true}, Rescan: true,
			})
		}
	}
}

// emit queues one change without waiting. A host that is not reading loses the
// change, and the next listing it asks for is still correct.
func (f *files) emit(change connectorapi.FileChange) {
	select {
	case f.changes <- change:
	default:
		f.session.dropped.Add(1)
	}
}

// Send puts one file into the gcodes root over HTTP, and confirms the name and
// the length by listing the folder afterwards.
func (s *Session) Send(ctx context.Context, up connectorapi.Upload) error {
	f := s.files

	rel, err := f.relative(up.Path)
	if err != nil {
		return err
	}

	dir, name := path.Split(rel)
	dir = strings.Trim(dir, "/")

	if name == "" {
		return connectorapi.PathNotAllowed.New("the path names no file").With(fault.StringParam("path", up.Path))
	}

	if !f.sending.CompareAndSwap(false, true) {
		return connectorapi.TransferBusy.New("the printer is already taking a file").
			With(fault.StringParam("path", up.Path)).
			Wrap(connectorapi.ErrTransferBusy)
	}

	defer f.sending.Store(false)

	progress := up.Progress
	if progress == nil {
		progress = func(connectorapi.Progress) {}
	}

	total := up.Content.Size()

	if err = f.upload(ctx, up, dir, name, total, progress); err != nil {
		return err
	}

	found, err := s.confirm(ctx, dir, name)
	if err != nil {
		progress(connectorapi.Progress{
			Total: total, Sent: total,
			Warning: "the file was sent, and the folder could not be read back to confirm it",
		})

		return nil //nolint:nilerr // the transfer finished; only the readback failed, which is a warning.
	}

	if found != strconv.FormatInt(total, 10) {
		return connectorapi.TransferNotOnStorage.New("the file is not on the printer as it was sent").
			With(fault.StringParam("path", up.Path),
				fault.IntParam("expected_size", total),
				fault.StringParam("found", found))
	}

	progress(connectorapi.Progress{Total: total, Sent: total, Received: total, Known: true})

	return nil
}

// upload streams the multipart body Moonraker's upload route reads. The body's
// length is known in advance, so no chunked encoding is needed through a proxy.
func (f *files) upload(
	ctx context.Context, up connectorapi.Upload, dir, name string, total int64, progress func(connectorapi.Progress),
) error {
	var head bytes.Buffer

	form := multipart.NewWriter(&head)
	_ = form.WriteField("root", gcodesRoot)

	if dir != "" {
		_ = form.WriteField("path", dir)
	}

	if _, err := form.CreateFormFile("file", name); err != nil {
		return fmt.Errorf("klipper: build the upload: %w", err)
	}

	prefix := bytes.Clone(head.Bytes())
	head.Reset()

	if err := form.Close(); err != nil {
		return fmt.Errorf("klipper: build the upload: %w", err)
	}

	trailer := bytes.Clone(head.Bytes())

	content, err := up.Content.Open(ctx, 0)
	if err != nil {
		return fmt.Errorf("klipper: open the file to send: %w", err)
	}

	defer func() { _ = content.Close() }()

	counted := &countingReader{reader: io.LimitReader(content, total), total: total, report: progress}
	body := io.MultiReader(bytes.NewReader(prefix), counted, bytes.NewReader(trailer))

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base+"/server/files/upload", body)
	if err != nil {
		return fmt.Errorf("klipper: build the upload: %w", err)
	}

	request.ContentLength = int64(len(prefix)) + total + int64(len(trailer))
	request.Header.Set("Content-Type", form.FormDataContentType())
	f.authorize(request)

	progress(connectorapi.Progress{Total: total})

	response, err := f.client.Do(request)
	if err != nil {
		return connectorapi.TransferFailed.New("the transfer did not finish").
			With(fault.StringParam("path", up.Path), fault.StringParam("reason", err.Error())).
			Wrap(fmt.Errorf("%w: %w", connectorapi.ErrTransferFailed, err))
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode >= http.StatusMultipleChoices {
		reason := httpReason(response)

		return connectorapi.TransferRefused.New("the printer would not take the file").
			With(fault.StringParam("path", up.Path),
				fault.StringParam("machine_reason", reason),
				fault.StringParam("reason", reason)).
			Wrap(connectorapi.ErrTransferRefused)
	}

	if counted.sent != total {
		return connectorapi.TransferFailed.New("the file ended before its length").
			With(fault.StringParam("path", up.Path)).
			Wrap(connectorapi.ErrTransferFailed)
	}

	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxErrorBody))

	return nil
}

// httpReason is Moonraker's own message from an HTTP refusal.
func httpReason(response *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))

	var answer struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if json.Unmarshal(raw, &answer) == nil && answer.Error.Message != "" {
		return answer.Error.Message
	}

	if text := strings.TrimSpace(string(raw)); text != "" {
		return text
	}

	return response.Status
}

// confirm lists the folder a file was sent to and answers the size it lists
// the file at, "absent" when it lists none, or "folder".
func (s *Session) confirm(ctx context.Context, dir, name string) (string, error) {
	var reply directoryReply

	refusal, err := s.callInto(ctx, methodGetDirectory, map[string]any{"path": rootPath(dir)}, &reply)
	if err != nil {
		return "", err
	}

	if refusal != nil {
		return "", errors.New(refusal.Message)
	}

	if dir == "" {
		s.files.remember(reply)
	}

	if slices.ContainsFunc(reply.Dirs, func(d directoryEntry) bool { return d.Dirname == name }) {
		return "folder", nil
	}

	for _, file := range reply.Files {
		if file.Filename == name {
			return strconv.FormatInt(file.Size, 10), nil
		}
	}

	return "absent", nil
}

// countingReader counts the bytes of the file that went out and reports them.
type countingReader struct {
	reader   io.Reader
	total    int64
	sent     int64
	reported int64
	report   func(connectorapi.Progress)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.sent += int64(n)

	if c.sent-c.reported >= progressStep || (err != nil && c.sent != c.reported) {
		c.reported = c.sent
		c.report(connectorapi.Progress{Total: c.total, Sent: c.sent})
	}

	return n, err //nolint:wrapcheck // io.Reader must return io.EOF unwrapped.
}
