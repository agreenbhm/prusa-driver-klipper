package klipper

import "encoding/json"

// jsonrpcVersion is the version every frame on this wire carries.
const jsonrpcVersion = "2.0"

// The JSON-RPC methods this driver calls. Moonraker serves the same names over
// the websocket and over HTTP.
const (
	methodPrinterInfo      = "printer.info"
	methodObjectsList      = "printer.objects.list"
	methodObjectsQuery     = "printer.objects.query"
	methodObjectsSubscribe = "printer.objects.subscribe"
	methodPrintStart       = "printer.print.start"
	methodPrintPause       = "printer.print.pause"
	methodPrintResume      = "printer.print.resume"
	methodPrintCancel      = "printer.print.cancel"
	methodGcodeScript      = "printer.gcode.script"
	methodFirmwareRestart  = "printer.firmware_restart"
	methodRestart          = "printer.restart"

	methodPowerDevices = "machine.device_power.devices"
	methodPowerPost    = "machine.device_power.post_device"

	methodGetDirectory    = "server.files.get_directory"
	methodFileMetadata    = "server.files.metadata"
	methodPostDirectory   = "server.files.post_directory"
	methodDeleteDirectory = "server.files.delete_directory"
	methodDeleteFile      = "server.files.delete_file"
)

// The notifications Moonraker sends without being asked.
const (
	// notifyStatusUpdate carries a subscription. Its params are the changed
	// status and the Klipper event time, in that order.
	notifyStatusUpdate = "notify_status_update"
	// notifyKlippyDisconnected says Klipper went away. Moonraker clears every
	// subscription then, without closing the websocket, so the session ends
	// and the host dials again.
	notifyKlippyDisconnected = "notify_klippy_disconnected"
	// notifyKlippyShutdown says Klipper stopped on an error. The webhooks
	// object reports the same in its next update.
	notifyKlippyShutdown = "notify_klippy_shutdown"
	// notifyFilelistChanged says a file or a folder changed under a root.
	notifyFilelistChanged = "notify_filelist_changed"
	// notifyPowerChanged says a power device switched. Its params are the
	// devices that changed.
	notifyPowerChanged = "notify_power_changed"
)

// gcodesRoot is the Moonraker root that holds print files. It is the only root
// this driver exposes.
const gcodesRoot = "gcodes"

// The Klipper objects this driver reads. Each one is a section in the machine's
// own configuration, so a machine that has not got the section does not report
// the object either.
const (
	objectWebhooks      = "webhooks"
	objectPrintStats    = "print_stats"
	objectDisplayStatus = "display_status"
	objectVirtualSD     = "virtual_sdcard"
	objectExtruder      = "extruder"
	objectHeaterBed     = "heater_bed"
	objectToolhead      = "toolhead"
	objectGcodeMove     = "gcode_move"
	objectPauseResume   = "pause_resume"
	objectConfigFile    = "configfile"
	objectExcludeObject = "exclude_object"
	objectBedMesh       = "bed_mesh"
)

// The chamber objects this driver looks for, in order of preference. A heater
// can be set; a sensor can only be read.
const (
	objectChamberHeater = "heater_generic chamber"
	objectChamberSensor = "temperature_sensor chamber"
)

// The macros this driver calls for verbs Klipper has no command of its own for.
// Most printer configurations define them, and a machine that does not is not
// offered the verb.
const (
	macroLoadFilament   = "gcode_macro LOAD_FILAMENT"
	macroUnloadFilament = "gcode_macro UNLOAD_FILAMENT"
)

// subscribed is every object whose readings become state patches, in the order
// a subscription names them. The extruders after the first and the chamber are
// added when the machine has them.
//
//nolint:gochecknoglobals // a fixed list, read-only after init.
var subscribed = []string{
	objectWebhooks, objectPrintStats, objectDisplayStatus, objectVirtualSD,
	objectExtruder, objectHeaterBed, objectToolhead, objectGcodeMove,
	objectExcludeObject,
}

// rpcRequest is one call on its way to the machine.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	ID      uint64 `json:"id"`
}

// rpcFrame is anything the machine sends. A frame carrying an id answers a
// call; one carrying a method and no id is a notification.
type rpcFrame struct {
	ID     *uint64         `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Params json.RawMessage `json:"params"`
}

// rpcError is the machine's own refusal, passed on as it spelled it.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// printerInfo is what printer.info answers.
type printerInfo struct {
	State           string `json:"state"`
	StateMessage    string `json:"state_message"`
	Hostname        string `json:"hostname"`
	SoftwareVersion string `json:"software_version"`
}

// objectList is what printer.objects.list answers.
type objectList struct {
	Objects []string `json:"objects"`
}

// statusReply is what a query and a subscription both answer.
type statusReply struct {
	EventTime float64                    `json:"eventtime"`
	Status    map[string]json.RawMessage `json:"status"`
}

// configDump is the machine's own configuration as Klipper parsed it, keyed by
// section.
type configDump struct {
	Settings map[string]json.RawMessage `json:"settings"`
}

// heaterConfig is the pair of bounds a Klipper heater section declares.
type heaterConfig struct {
	MinTemp *float64 `json:"min_temp"`
	MaxTemp *float64 `json:"max_temp"`
}

// directoryReply is what server.files.get_directory answers.
type directoryReply struct {
	Dirs      []directoryEntry `json:"dirs"`
	Files     []fileEntry      `json:"files"`
	DiskUsage *diskUsage       `json:"disk_usage"`
	RootInfo  *rootInfo        `json:"root_info"`
}

// directoryEntry is one folder in a listing.
type directoryEntry struct {
	Dirname     string  `json:"dirname"`
	Modified    float64 `json:"modified"`
	Size        int64   `json:"size"`
	Permissions string  `json:"permissions"`
}

// fileEntry is one file in a listing.
type fileEntry struct {
	Filename    string  `json:"filename"`
	Modified    float64 `json:"modified"`
	Size        int64   `json:"size"`
	Permissions string  `json:"permissions"`
}

// diskUsage is the space on the disk that holds a root, in bytes.
type diskUsage struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
	Free  int64 `json:"free"`
}

// rootInfo names a root and says whether it can be written.
type rootInfo struct {
	Name        string `json:"name"`
	Permissions string `json:"permissions"`
}

// fileMetadata is the part of server.files.metadata this driver reads. Moonraker
// reads it out of the slicer's comments in the file.
type fileMetadata struct {
	Size          int64       `json:"size"`
	Modified      float64     `json:"modified"`
	EstimatedTime *float64    `json:"estimated_time"`
	LayerHeight   *float64    `json:"layer_height"`
	FilamentType  string      `json:"filament_type"`
	Thumbnails    []thumbnail `json:"thumbnails"`
}

// thumbnail is one picture Moonraker extracted from a print file.
// RelativePath is relative to the folder that holds the print file.
type thumbnail struct {
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Size         int64  `json:"size"`
	RelativePath string `json:"relative_path"`
}

// filelistChange is one notify_filelist_changed entry.
type filelistChange struct {
	Action string       `json:"action"`
	Item   filelistItem `json:"item"`
}

// filelistItem is the file or folder a change names. Path is relative to Root.
type filelistItem struct {
	Path     string  `json:"path"`
	Root     string  `json:"root"`
	Size     int64   `json:"size"`
	Modified float64 `json:"modified"`
}

// powerDevice is one device Moonraker's [power] sections declare, such as a
// smart plug that feeds the printer's mainboard.
type powerDevice struct {
	Device string `json:"device"`
	Status string `json:"status"`
}

// powerList is what machine.device_power.devices answers.
type powerList struct {
	Devices []powerDevice `json:"devices"`
}

// uploadReply is what POST /server/files/upload answers.
type uploadReply struct {
	Item filelistItem `json:"item"`
}

// objectsParam is the params both query and subscribe take. A nil value asks
// for every field of that object.
func objectsParam(objects []string) map[string]any {
	asked := make(map[string]any, len(objects))
	for _, object := range objects {
		asked[object] = nil
	}

	return map[string]any{"objects": asked}
}
