// Package klippersim is a Moonraker host with no Klipper behind it. It answers
// the JSON-RPC calls and the HTTP file routes the real one answers, so the
// driver can be driven end to end without a machine on the bench.
//
// It is written from Moonraker's published API, not recorded from a real host.
// It proves the client and the contract; whether a real Moonraker spells a
// field the same way is settled only against a real machine.
package klippersim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"maps"
	"net"
	"net/http"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	websocketPath     = "/websocket"
	readHeaderTimeout = 5 * time.Second
	jsonrpcVersion    = "2.0"
	klipperTime       = 578243.57824499
	methodNotFound    = -32601
	requestRefused    = 400
	notFound          = 404
	diskTotal         = 32 << 30
)

// Options shapes one fake machine. The zero value is a complete machine.
type Options struct {
	// Hostname is what printer.info reports.
	Hostname string
	// SoftwareVersion is the Klipper version printer.info reports.
	SoftwareVersion string
	// KlippyState is printer.info's state. Empty is "ready".
	KlippyState string
	// Objects is what printer.objects.list reports. Nil is DefaultObjects.
	Objects []string
	// Status is every object's fields. Nil is DefaultStatus.
	Status map[string]map[string]any
	// Refused maps a method to the error message it is answered with.
	Refused map[string]string
	// APIKey, when set, is required on the websocket and on HTTP.
	APIKey string
	// Files is the gcodes folder: path to content. Nil is DefaultFiles.
	Files map[string]string
	// Power is the Moonraker power devices and their status. Nil is one
	// device, "printer", that is off. An empty map is a Moonraker with no
	// [power] section.
	Power map[string]string
	// Addr is where to listen. Empty is an ephemeral loopback port.
	Addr string
}

// DefaultObjects is a complete machine's object list.
func DefaultObjects() []string {
	return []string{
		"configfile", "display_status", "extruder", "gcode", "gcode_move",
		"heater_bed", "heaters", "pause_resume", "print_stats", "toolhead",
		"virtual_sdcard", "webhooks", "exclude_object", "bed_mesh",
		"gcode_macro LOAD_FILAMENT", "gcode_macro UNLOAD_FILAMENT",
		"heater_generic chamber",
	}
}

// DefaultStatus is a complete machine standing idle.
func DefaultStatus() map[string]map[string]any {
	return map[string]map[string]any{
		"webhooks": {"state": "ready", "state_message": "Printer is ready"},
		"print_stats": {
			"state": "standby", "filename": "", "print_duration": 0.0, "total_duration": 0.0,
			"filament_used": 0.0,
		},
		"display_status":         {"progress": 0.0, "message": nil},
		"virtual_sdcard":         {"progress": 0.0, "is_active": false, "file_position": 0},
		"extruder":               {"temperature": 24.5, "target": 0.0, "power": 0.0},
		"heater_bed":             {"temperature": 23.9, "target": 0.0, "power": 0.0},
		"heater_generic chamber": {"temperature": 22.0, "target": 0.0, "power": 0.0},
		"toolhead":               {"position": []any{0.0, 0.0, 0.0, 0.0}, "homed_axes": "", "extruder": "extruder"},
		"gcode_move":             {"speed_factor": 1.0, "extrude_factor": 1.0},
		"exclude_object":         {"objects": []any{}, "excluded_objects": []any{}, "current_object": nil},
		"configfile": {
			"settings": map[string]any{
				"extruder":               map[string]any{"min_temp": 0.0, "max_temp": 300.0, "nozzle_diameter": 0.4},
				"heater_bed":             map[string]any{"min_temp": 0.0, "max_temp": 120.0},
				"heater_generic chamber": map[string]any{"min_temp": 0.0, "max_temp": 70.0},
				"printer":                map[string]any{"kinematics": "corexy"},
				"stepper_x":              map[string]any{"position_min": 0.0, "position_max": 300.0},
				"stepper_y":              map[string]any{"position_min": -5.0, "position_max": 300.0},
				"stepper_z":              map[string]any{"position_max": 280.0},
			},
		},
	}
}

// DefaultFiles is a gcodes folder with one file in the root, one in a
// subfolder, and a thumbnail for the first.
func DefaultFiles() map[string]string {
	return map[string]string{
		"benchy.gcode":               "; estimated printing time\nG28\n",
		"parts/bracket.gcode":        "G28\nG1 X10\n",
		".thumbs/benchy-300x300.png": picture(300),
		".thumbs/benchy-32x32.png":   picture(32),
	}
}

// picture is a real PNG of one colour, size pixels square.
func picture(size int) string {
	canvas := image.NewRGBA(image.Rect(0, 0, size, size))
	for i := range canvas.Pix {
		canvas.Pix[i] = 0xff
	}

	var out bytes.Buffer
	_ = png.Encode(&out, canvas)

	return out.String()
}

// Host is one running fake machine.
type Host struct {
	listener net.Listener
	server   *http.Server
	upgrader websocket.Upgrader

	hostname string
	version  string
	klippy   string
	objects  []string
	apiKey   string

	mu      sync.Mutex
	status  map[string]map[string]any
	refused map[string]string
	files   map[string]string
	dirs    map[string]bool
	conns   []*link
	calls   []string
	scripts []string
	printed []string
	power   map[string]string
}

type link struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (l *link) write(frame any) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.conn.WriteJSON(frame); err != nil {
		return fmt.Errorf("klippersim: write a frame: %w", err)
	}

	return nil
}

// Start binds a port and serves one fake machine until Close.
func Start(ctx context.Context, opts Options) (*Host, error) {
	var binding net.ListenConfig

	listener, err := binding.Listen(ctx, "tcp", orElse(opts.Addr, "127.0.0.1:0"))
	if err != nil {
		return nil, fmt.Errorf("klippersim: bind a port: %w", err)
	}

	host := &Host{
		listener: listener,
		hostname: orElse(opts.Hostname, "voron-bench"),
		version:  orElse(opts.SoftwareVersion, "v0.12.0-284-gfb1b4e39"),
		klippy:   orElse(opts.KlippyState, "ready"),
		objects:  opts.Objects,
		status:   opts.Status,
		refused:  maps.Clone(opts.Refused),
		apiKey:   opts.APIKey,
		files:    maps.Clone(opts.Files),
		dirs:     map[string]bool{},
	}

	if host.objects == nil {
		host.objects = DefaultObjects()
	}

	if host.status == nil {
		host.status = DefaultStatus()
	}

	host.power = maps.Clone(opts.Power)
	if opts.Power == nil {
		host.power = map[string]string{"printer": "off"}
	}

	if host.files == nil {
		host.files = DefaultFiles()
	}

	for file := range host.files {
		for dir := path.Dir(file); dir != "."; dir = path.Dir(dir) {
			host.dirs[dir] = true
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc(websocketPath, host.serve)
	mux.HandleFunc("POST /server/files/upload", host.upload)
	mux.HandleFunc("GET /server/files/gcodes/", host.download)

	host.server = &http.Server{Handler: mux, ReadHeaderTimeout: readHeaderTimeout}

	go func() { _ = host.server.Serve(listener) }()

	return host, nil
}

// Address is where the host listens.
func (h *Host) Address() string { return h.listener.Addr().String() }

// Close stops serving and drops every connection.
func (h *Host) Close() error {
	h.HangUp()

	if err := h.server.Close(); err != nil {
		return fmt.Errorf("klippersim: stop serving: %w", err)
	}

	return nil
}

// HangUp drops every open websocket, as a Moonraker restart does.
func (h *Host) HangUp() {
	h.mu.Lock()
	dropped := h.conns
	h.conns = nil
	h.mu.Unlock()

	for _, held := range dropped {
		_ = held.conn.Close()
	}
}

// Calls is every method called, in order.
func (h *Host) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.calls)
}

// Called reports whether a method was called.
func (h *Host) Called(method string) bool { return slices.Contains(h.Calls(), method) }

// Scripts is every G-code script received, in order.
func (h *Host) Scripts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.scripts)
}

// Printed is every file printer.print.start was asked for.
func (h *Host) Printed() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return slices.Clone(h.printed)
}

// File is one file's content, and whether it exists.
func (h *Host) File(name string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	content, ok := h.files[name]

	return content, ok
}

// SetKlippyState changes what printer.info answers, as Klipper does while it
// restarts.
func (h *Host) SetKlippyState(state string) {
	h.mu.Lock()
	h.klippy = state
	h.mu.Unlock()
}

// Notify sends a notification with the given params to every connection.
func (h *Host) Notify(method string, params any) {
	h.broadcast(map[string]any{"jsonrpc": jsonrpcVersion, "method": method, "params": params})
}

// Push changes the status and sends the change as a subscription update.
func (h *Host) Push(status map[string]map[string]any) {
	h.mu.Lock()

	for object, fields := range status {
		if h.status[object] == nil {
			h.status[object] = map[string]any{}
		}

		maps.Copy(h.status[object], fields)
	}

	h.mu.Unlock()

	h.Notify("notify_status_update", []any{status, klipperTime})
}

func (h *Host) broadcast(frame any) {
	h.mu.Lock()
	open := slices.Clone(h.conns)
	h.mu.Unlock()

	for _, held := range open {
		_ = held.write(frame)
	}
}

func (h *Host) authorized(r *http.Request) bool {
	return h.apiKey == "" || r.Header.Get("X-Api-Key") == h.apiKey
}

func (h *Host) serve(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)

		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	open := &link{conn: conn}

	h.mu.Lock()
	h.conns = append(h.conns, open)
	h.mu.Unlock()

	defer func() { _ = conn.Close() }()

	for {
		var request struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     *uint64         `json:"id"`
		}

		if err = conn.ReadJSON(&request); err != nil {
			return
		}

		if request.ID == nil {
			continue
		}

		h.record(request.Method)

		if err = open.write(h.answer(*request.ID, request.Method, request.Params)); err != nil {
			return
		}
	}
}

func (h *Host) record(method string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.calls = append(h.calls, method)
}

//nolint:cyclop,funlen // one switch over the methods a fake answers.
func (h *Host) answer(id uint64, method string, params json.RawMessage) map[string]any {
	if message, refused := h.refusal(method); refused {
		return errorFrame(id, requestRefused, message)
	}

	var p struct {
		Path     string `json:"path"`
		Filename string `json:"filename"`
		Script   string `json:"script"`
		Force    bool   `json:"force"`
	}

	_ = json.Unmarshal(params, &p)

	switch method {
	case "printer.info":
		h.mu.Lock()
		klippy := h.klippy
		h.mu.Unlock()

		return resultFrame(id, map[string]any{
			"state": klippy, "state_message": "Printer is " + klippy,
			"hostname": h.hostname, "software_version": h.version,
		})
	case "printer.objects.list":
		return resultFrame(id, map[string]any{"objects": h.objects})
	case "printer.objects.query", "printer.objects.subscribe":
		return resultFrame(id, map[string]any{"eventtime": klipperTime, "status": h.reading(params)})
	case "printer.gcode.script":
		h.mu.Lock()
		h.scripts = append(h.scripts, p.Script)
		h.mu.Unlock()

		return resultFrame(id, "ok")
	case "printer.print.start":
		if _, ok := h.File(p.Filename); !ok {
			return errorFrame(id, requestRefused, "File not found: "+p.Filename)
		}

		h.mu.Lock()
		h.printed = append(h.printed, p.Filename)
		h.mu.Unlock()

		return resultFrame(id, "ok")
	case "printer.print.pause", "printer.print.resume", "printer.print.cancel", "printer.firmware_restart",
		"printer.restart":
		return resultFrame(id, "ok")
	case "machine.device_power.devices":
		if len(h.power) == 0 {
			return errorFrame(id, notFound, "Component power not loaded")
		}

		h.mu.Lock()
		var devices []map[string]any
		for _, name := range slices.Sorted(maps.Keys(h.power)) {
			devices = append(devices, map[string]any{
				"device": name, "status": h.power[name], "locked_while_printing": true, "type": "gpio",
			})
		}
		h.mu.Unlock()

		return resultFrame(id, map[string]any{"devices": devices})
	case "machine.device_power.post_device":
		return h.switchPower(id, params)
	case "server.files.get_directory":
		return h.directory(id, p.Path)
	case "server.files.metadata":
		return h.metadata(id, p.Filename)
	case "server.files.post_directory":
		return h.mkdir(id, p.Path)
	case "server.files.delete_file":
		return h.deleteFile(id, p.Path)
	case "server.files.delete_directory":
		return h.deleteDir(id, p.Path, p.Force)
	default:
		return errorFrame(id, methodNotFound, "Method "+method+" is not available")
	}
}

// underRoot strips "gcodes/" from a root path. ok is false for another root.
func underRoot(rootPath string) (string, bool) {
	rootPath = strings.Trim(rootPath, "/")
	if rootPath == "gcodes" {
		return "", true
	}

	rel, found := strings.CutPrefix(rootPath, "gcodes/")

	return rel, found
}

func (h *Host) directory(id uint64, rootPath string) map[string]any {
	rel, ok := underRoot(rootPath)

	h.mu.Lock()
	defer h.mu.Unlock()

	if !ok || (rel != "" && !h.dirs[rel]) {
		return errorFrame(id, notFound, "Directory does not exist ("+rootPath+")")
	}

	dirs, files := []map[string]any{}, []map[string]any{}

	for dir := range h.dirs {
		if path.Dir(dir) == orElse(rel, ".") {
			dirs = append(dirs, map[string]any{"dirname": path.Base(dir), "modified": 1700000000.5, "size": 4096, "permissions": "rw"})
		}
	}

	for file, content := range h.files {
		if path.Dir(file) == orElse(rel, ".") {
			files = append(files, map[string]any{
				"filename": path.Base(file), "modified": 1700000000.5, "size": len(content), "permissions": "rw",
			})
		}
	}

	sort.Slice(dirs, func(i, j int) bool { return dirs[i]["dirname"].(string) < dirs[j]["dirname"].(string) })
	sort.Slice(files, func(i, j int) bool { return files[i]["filename"].(string) < files[j]["filename"].(string) })

	return resultFrame(id, map[string]any{
		"dirs": dirs, "files": files,
		"disk_usage": map[string]any{"total": diskTotal, "used": 1 << 30, "free": diskTotal - 1<<30},
		"root_info":  map[string]any{"name": "gcodes", "permissions": "rw"},
	})
}

func (h *Host) metadata(id uint64, name string) map[string]any {
	content, ok := h.File(name)
	if !ok {
		return errorFrame(id, notFound, "Metadata not available for <"+name+">")
	}

	meta := map[string]any{
		"size": len(content), "modified": 1700000000.5, "slicer": "PrusaSlicer",
		"estimated_time": 3725.0, "layer_height": 0.2, "filament_type": "PLA",
	}

	stem := strings.TrimSuffix(path.Base(name), path.Ext(name))

	var thumbs []map[string]any

	for _, size := range []string{"32x32", "300x300"} {
		thumb := path.Join(path.Dir(name), ".thumbs", stem+"-"+size+".png")
		if data, has := h.File(thumb); has {
			width := 32
			if size == "300x300" {
				width = 300
			}

			thumbs = append(thumbs, map[string]any{
				"width": width, "height": width, "size": len(data),
				"relative_path": ".thumbs/" + stem + "-" + size + ".png",
			})
		}
	}

	if thumbs != nil {
		meta["thumbnails"] = thumbs
	}

	return resultFrame(id, meta)
}

func (h *Host) mkdir(id uint64, rootPath string) map[string]any {
	rel, ok := underRoot(rootPath)
	if !ok || rel == "" {
		return errorFrame(id, requestRefused, "Invalid path")
	}

	h.mu.Lock()

	if h.dirs[rel] {
		h.mu.Unlock()

		return errorFrame(id, requestRefused, "Directory already exists: "+rel)
	}

	h.dirs[rel] = true
	h.mu.Unlock()

	h.fileEvent("create_dir", rel, 0)

	return resultFrame(id, map[string]any{"item": map[string]any{"path": rel, "root": "gcodes"}, "action": "create_dir"})
}

func (h *Host) deleteFile(id uint64, rootPath string) map[string]any {
	rel, _ := underRoot(rootPath)

	h.mu.Lock()

	if _, ok := h.files[rel]; !ok {
		h.mu.Unlock()

		return errorFrame(id, notFound, "File does not exist: "+rel)
	}

	delete(h.files, rel)
	h.mu.Unlock()

	h.fileEvent("delete_file", rel, 0)

	return resultFrame(id, map[string]any{"item": map[string]any{"path": rel, "root": "gcodes"}, "action": "delete_file"})
}

func (h *Host) deleteDir(id uint64, rootPath string, force bool) map[string]any {
	rel, _ := underRoot(rootPath)

	h.mu.Lock()

	if rel == "" || !h.dirs[rel] {
		h.mu.Unlock()

		return errorFrame(id, notFound, "Directory does not exist: "+rel)
	}

	for file := range h.files {
		if strings.HasPrefix(file, rel+"/") {
			if !force {
				h.mu.Unlock()

				return errorFrame(id, requestRefused, "Directory not empty: "+rel)
			}

			delete(h.files, file)
		}
	}

	for dir := range h.dirs {
		if dir == rel || strings.HasPrefix(dir, rel+"/") {
			delete(h.dirs, dir)
		}
	}

	h.mu.Unlock()

	h.fileEvent("delete_dir", rel, 0)

	return resultFrame(id, map[string]any{"item": map[string]any{"path": rel, "root": "gcodes"}, "action": "delete_dir"})
}

func (h *Host) switchPower(id uint64, params json.RawMessage) map[string]any {
	var p struct {
		Device string `json:"device"`
		Action string `json:"action"`
	}

	_ = json.Unmarshal(params, &p)

	h.mu.Lock()
	_, known := h.power[p.Device]
	if known {
		h.power[p.Device] = p.Action
	}
	h.mu.Unlock()

	if !known {
		return errorFrame(id, notFound, "No valid power device named "+p.Device)
	}

	h.Notify("notify_power_changed", []any{map[string]any{"device": p.Device, "status": p.Action}})

	return resultFrame(id, map[string]any{p.Device: p.Action})
}

// PowerStatus is one power device's status.
func (h *Host) PowerStatus(device string) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.power[device]
}

func (h *Host) fileEvent(action, rel string, size int) {
	h.Notify("notify_filelist_changed", []any{map[string]any{
		"action": action,
		"item":   map[string]any{"path": rel, "root": "gcodes", "size": size, "modified": 1700000000.5},
	}})
}

// upload is POST /server/files/upload.
func (h *Host) upload(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, `{"error":{"code":401,"message":"Unauthorized"}}`, http.StatusUnauthorized)

		return
	}

	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, `{"error":{"code":400,"message":"bad form"}}`, http.StatusBadRequest)

		return
	}

	if root := r.FormValue("root"); root != "" && root != "gcodes" {
		http.Error(w, `{"error":{"code":400,"message":"Invalid root"}}`, http.StatusBadRequest)

		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":{"code":400,"message":"No file"}}`, http.StatusBadRequest)

		return
	}

	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, `{"error":{"code":500,"message":"read"}}`, http.StatusInternalServerError)

		return
	}

	rel := path.Join(strings.Trim(r.FormValue("path"), "/"), header.Filename)

	h.mu.Lock()
	h.files[rel] = string(data)

	for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
		h.dirs[dir] = true
	}

	h.mu.Unlock()

	h.fileEvent("create_file", rel, len(data))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"item": map[string]any{"path": rel, "root": "gcodes"}, "action": "create_file",
	})
}

// download is GET /server/files/gcodes/<path>.
func (h *Host) download(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)

		return
	}

	rel := strings.TrimPrefix(r.URL.Path, "/server/files/gcodes/")

	content, ok := h.File(rel)
	if !ok {
		http.NotFound(w, r)

		return
	}

	_, _ = io.WriteString(w, content)
}

func (h *Host) refusal(method string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	message, refused := h.refused[method]

	return message, refused
}

func (h *Host) reading(params json.RawMessage) map[string]map[string]any {
	var asked struct {
		Objects map[string]json.RawMessage `json:"objects"`
	}

	_ = json.Unmarshal(params, &asked)

	h.mu.Lock()
	defer h.mu.Unlock()

	out := make(map[string]map[string]any, len(asked.Objects))

	for object := range asked.Objects {
		fields, held := h.status[object]
		if !held || !slices.Contains(h.objects, object) {
			continue
		}

		out[object] = maps.Clone(fields)
	}

	return out
}

func resultFrame(id uint64, result any) map[string]any {
	return map[string]any{"jsonrpc": jsonrpcVersion, "id": id, "result": result}
}

func errorFrame(id uint64, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": jsonrpcVersion, "id": id,
		"error": map[string]any{"code": code, "message": message},
	}
}

func orElse(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
