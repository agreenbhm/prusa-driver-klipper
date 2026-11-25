package klipper_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agreenbhm/prusa-driver-klipper"
	"github.com/agreenbhm/prusa-driver-klipper/klippersim"

	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
	"github.com/prusa3d/prusa-connect/sdk/connectorapi/connectorapitest"
)

// TestContract runs the published conformance suite against this driver.
func TestContract(t *testing.T) {
	t.Parallel()

	connectorapitest.RunDriverContract(t, func(t *testing.T) connectorapitest.Subject {
		return connectorapitest.Subject{
			Driver:      klipper.NewDriver(0),
			Endpoint:    connectorapi.Endpoint{Address: startMachine(t, klippersim.Options{}).Address()},
			Connectable: true,
			SafeVerb:    klipper.VerbSendStateInfo,
		}
	})
}

func TestTheDriverIsRegistered(t *testing.T) {
	t.Parallel()

	for _, descriptor := range connectorapi.Descriptors() {
		if descriptor.Name() == klipper.Type.String() {
			return
		}
	}

	t.Fatalf("%s registered nothing", klipper.Type)
}

func TestTheProbeOffersWhatTheMachineHas(t *testing.T) {
	t.Parallel()

	full := connect(t, startMachine(t, klippersim.Options{}))
	caps, _ := full.Capabilities()

	for _, verb := range []connectorapi.Capability{
		klipper.VerbStartPrint, klipper.VerbPausePrint, klipper.VerbCancelObject, klipper.VerbMove,
		klipper.VerbHome, klipper.VerbSetSpeed, klipper.VerbSendGcode, klipper.VerbMeshLeveling,
		klipper.VerbLoadFilament, klipper.VerbChamberTarget, klipper.FeatureUpload, klipper.VerbSendInfo,
		klipper.VerbSetIdle,
	} {
		if !caps.Has(verb) {
			t.Errorf("a complete machine is not offered %s", verb)
		}
	}

	if limit := caps[klipper.ArgNozzleTarget].Limit; limit != 300 {
		t.Errorf("the nozzle ceiling is %v, and the machine's max_temp is 300", limit)
	}

	bare := connect(t, startMachine(t, klippersim.Options{Objects: []string{
		"configfile", "extruder", "heater_bed", "print_stats", "toolhead", "gcode_move", "webhooks",
	}}))
	caps, _ = bare.Capabilities()

	for _, verb := range []connectorapi.Capability{
		klipper.VerbPausePrint, klipper.VerbStartPrint, klipper.VerbCancelObject,
		klipper.VerbMeshLeveling, klipper.VerbLoadFilament, klipper.VerbChamberTarget,
	} {
		if caps.Has(verb) {
			t.Errorf("a machine without the section behind %s is offered it", verb)
		}
	}
}

func TestCommandsBecomeKlipperGcode(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{})
	session := connect(t, host)

	cases := []struct {
		verb connectorapi.Capability
		args map[string]string
		want string
	}{
		{klipper.VerbHome, map[string]string{"axis": "xy"}, "G28 X Y"},
		{klipper.VerbHome, nil, "G28"},
		{klipper.VerbMove, map[string]string{"x": "10", "y": "-0.1", "feedrate": "3000"},
			"SAVE_GCODE_STATE NAME=prusa_connect_jog\nG91\nG1 X10 Y-0.1 F3000\nRESTORE_GCODE_STATE NAME=prusa_connect_jog"},
		{klipper.VerbMoveZ, map[string]string{"distance": "1", "feedrate": "600"},
			"SAVE_GCODE_STATE NAME=prusa_connect_jog\nG91\nG1 Z1 F600\nRESTORE_GCODE_STATE NAME=prusa_connect_jog"},
		{klipper.VerbSetSpeed, map[string]string{"speed": "150"}, "M220 S150"},
		{klipper.VerbSetFlow, map[string]string{"flow": "95"}, "M221 S95"},
		{klipper.VerbNozzleTarget, map[string]string{"nozzle_temperature": "215"}, "M104 S215"},
		{klipper.VerbNozzleTarget, map[string]string{"nozzle_temperature": "215", "tool": "0"},
			"SET_HEATER_TEMPERATURE HEATER=extruder TARGET=215"},
		{klipper.VerbBedTarget, map[string]string{"bed_temperature": "60"},
			"SET_HEATER_TEMPERATURE HEATER=heater_bed TARGET=60"},
		{klipper.VerbChamberTarget, map[string]string{"chamber.target_temp": "45"},
			"SET_HEATER_TEMPERATURE HEATER=chamber TARGET=45"},
		{klipper.VerbDisableSteppers, nil, "M84"},
		{klipper.VerbMeshLeveling, nil, "BED_MESH_CALIBRATE"},
		{klipper.VerbSetIdle, nil, "SDCARD_RESET_FILE"},
		{klipper.VerbSendGcode, map[string]string{"gcode": "G28\r\nM115"}, "G28\nM115"},
	}

	for _, c := range cases {
		result, err := session.Do(t.Context(), connectorapi.Command{Verb: c.verb, Args: c.args})
		if err != nil || !result.OK {
			t.Fatalf("%s %v: %v %+v", c.verb, c.args, err, result)
		}

		scripts := host.Scripts()
		if got := scripts[len(scripts)-1]; got != c.want {
			t.Errorf("%s %v sent %q, want %q", c.verb, c.args, got, c.want)
		}
	}

	result, err := session.Do(t.Context(), connectorapi.Command{
		Verb: klipper.VerbLoadFilament, Args: map[string]string{"filament": "PLA"},
	})
	if err != nil || !result.OK || !slices.Equal(result.Ignored, []string{"filament"}) {
		t.Errorf("load filament: %v %+v, want filament ignored", err, result)
	}
}

func TestAMachineRefusalIsAResult(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{Refused: map[string]string{
		"printer.gcode.script": "Must home axis first: 10.000 0.000 0.000 [0.000]",
	}})
	session := connect(t, host)

	result, err := session.Do(t.Context(), connectorapi.Command{
		Verb: klipper.VerbMove, Args: map[string]string{"x": "10", "feedrate": "3000"},
	})
	if err != nil {
		t.Fatalf("a refusal is not an error: %v", err)
	}

	if result.OK || !strings.Contains(result.MachineReason, "Must home") ||
		result.FaultCode != connectorapi.CommandRefused.Code().String() {
		t.Errorf("result %+v", result)
	}
}

func TestStartPrintNamesTheFileUnderTheGcodesRoot(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{})
	session := connect(t, host)

	for _, path := range []string{"usb/parts/bracket.gcode", "/usb/benchy.gcode"} {
		result, err := session.Do(t.Context(), connectorapi.Command{
			Verb: klipper.VerbStartPrint, Args: map[string]string{"path": path},
		})
		if err != nil || !result.OK {
			t.Fatalf("start %s: %v %+v", path, err, result)
		}
	}

	if got := host.Printed(); !slices.Equal(got, []string{"parts/bracket.gcode", "benchy.gcode"}) {
		t.Errorf("printed %v", got)
	}

	for _, path := range []string{"sd/benchy.gcode", "usb/../etc/passwd"} {
		_, err := session.Do(t.Context(), connectorapi.Command{
			Verb: klipper.VerbStartPrint, Args: map[string]string{"path": path},
		})
		if err == nil {
			t.Errorf("start %s was not refused", path)
		}
	}
}

func TestFiles(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{})
	session := connect(t, host)
	ctx := t.Context()

	storages, err := session.Storages(ctx)
	if err != nil || len(storages) != 1 || storages[0].Name != "usb" || !storages[0].FreeSpaceKnown {
		t.Fatalf("storages %+v %v", storages, err)
	}

	root, err := session.List(ctx, "")
	if err != nil || len(root.Entries) != 1 || root.Entries[0].Path != "usb" {
		t.Fatalf("root %+v %v", root, err)
	}

	top, err := session.List(ctx, "usb")
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, entry := range top.Entries {
		names = append(names, entry.Path)
	}

	if !slices.Equal(names, []string{"usb/parts", "usb/benchy.gcode"}) {
		t.Errorf("usb lists %v; the .thumbs folder must be hidden", names)
	}

	file, err := session.List(ctx, "usb/benchy.gcode")
	if err != nil {
		t.Fatal(err)
	}

	d := file.Directory
	if d.Folder || d.EstimatedSeconds != 3725 || d.LayerHeightMM != 0.2 || d.Material != "PLA" ||
		!bytes.HasPrefix(d.Preview, []byte("\x89PNG")) || len(d.Preview) < 100 {
		t.Errorf("file %+v", d)
	}

	if err = session.CreateFolder(ctx, "usb/new"); err != nil {
		t.Fatal(err)
	}

	if err = session.CreateFolder(ctx, "usb/new"); !errors.Is(err, connectorapi.ErrFileChangeRefused) {
		t.Errorf("a second create answered %v", err)
	}

	if err = session.DeleteFile(ctx, "usb/benchy.gcode"); err != nil {
		t.Fatal(err)
	}

	if _, has := host.File("benchy.gcode"); has {
		t.Error("the file is still there")
	}

	if err = session.DeleteFolder(ctx, "usb/parts"); err != nil {
		t.Fatal(err)
	}

	if _, has := host.File("parts/bracket.gcode"); has {
		t.Error("the folder's file is still there")
	}

	if _, err = session.List(ctx, "usb/../x"); err == nil {
		t.Error("a climbing path was listed")
	}
}

func TestUpload(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{})
	session := connect(t, host)

	content := bytes.Repeat([]byte("G1 X1\n"), 200_000)

	var reports []connectorapi.Progress

	err := session.Send(t.Context(), connectorapi.Upload{
		Path:     "usb/sliced/part.gcode",
		Content:  memory(content),
		Progress: func(p connectorapi.Progress) { reports = append(reports, p) },
	})
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := host.File("sliced/part.gcode"); got != string(content) {
		t.Errorf("the machine holds %d bytes, want %d", len(got), len(content))
	}

	last := reports[len(reports)-1]
	if !last.Known || last.Received != int64(len(content)) || len(reports) < 3 {
		t.Errorf("progress %d reports, last %+v", len(reports), last)
	}

	select {
	case change := <-session.FilesChanged():
		if change.File.Path != "usb/sliced/part.gcode" || change.Removed {
			t.Errorf("change %+v", change)
		}
	case <-time.After(2 * time.Second):
		t.Error("no file change was reported")
	}
}

func TestAnAPIKeyIsSent(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{APIKey: "secret"})
	driver := klipper.NewDriver(0)

	if _, err := driver.Connect(t.Context(), connectorapi.Endpoint{Address: host.Address()}, driver.Baseline()); err == nil {
		t.Fatal("a machine that requires a key was reached without one")
	}

	session, err := driver.Connect(t.Context(), connectorapi.Endpoint{
		Address: host.Address(), Options: map[string]string{"api_key": "secret"},
	}, driver.Baseline())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = session.Close() })

	files, _ := connectorapi.Uploads(session)
	if err = files.Send(t.Context(), connectorapi.Upload{Path: "usb/a.gcode", Content: memory([]byte("G28\n"))}); err != nil {
		t.Errorf("an upload with the key: %v", err)
	}
}

func TestTelemetry(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{})
	session := connect(t, host)
	tree := watch(t, session)

	tree.waitFor(t, "/state", `"IDLE"`)
	tree.waitFor(t, "/build_volume/y", `305`)
	tree.waitFor(t, "/temperature/chamber", `22`)

	host.Push(map[string]map[string]any{
		"print_stats":    {"state": "printing", "filename": "benchy.gcode", "print_duration": 600.0, "total_duration": 650.0},
		"display_status": {"progress": 0.25},
		"exclude_object": {"objects": []any{map[string]any{"name": "PART_1"}, map[string]any{"name": "PART 2"}}},
	})

	tree.waitFor(t, "/state", `"PRINTING"`)
	tree.waitFor(t, "/job/path", `"/usb/benchy.gcode"`)
	tree.waitFor(t, "/job/progress", `25`)
	tree.waitFor(t, "/job/time_remaining", `1800`)
	tree.waitFor(t, "/job/objects", `[{"id":0,"name":"PART_1","cancelled":false},{"id":1,"name":"PART 2","cancelled":false}]`)

	if tree.get("/job/id") == "" {
		t.Error("a print has no /job/id")
	}

	result, err := session.Do(t.Context(), connectorapi.Command{
		Verb: klipper.VerbCancelObject, Args: map[string]string{"id": "1"},
	})
	if err != nil || !result.OK {
		t.Fatalf("cancel object: %v %+v", err, result)
	}

	if scripts := host.Scripts(); scripts[len(scripts)-1] != "EXCLUDE_OBJECT NAME=PART 2" {
		t.Errorf("cancel object sent %q", scripts[len(scripts)-1])
	}

	host.Push(map[string]map[string]any{"exclude_object": {"excluded_objects": []any{"PART 2"}}})
	tree.waitFor(t, "/job/objects", `[{"id":0,"name":"PART_1","cancelled":false},{"id":1,"name":"PART 2","cancelled":true}]`)

	host.Push(map[string]map[string]any{"print_stats": {"state": "complete"}})
	tree.waitFor(t, "/state", `"FINISHED"`)
	tree.waitFor(t, "/job/outcome", `"finished"`)
	tree.waitFor(t, "/job/progress", `100`)

	host.Push(map[string]map[string]any{"webhooks": {"state": "shutdown", "state_message": "MCU 'mcu' shutdown"}})
	tree.waitFor(t, "/state", `"ERROR"`)
}

func TestPowerDevice(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{Power: map[string]string{"kp3s_plug": "off"}})
	s := connect(t, host)
	tree := watch(t, s)

	caps, _ := s.Capabilities()
	if !caps.Has(klipper.VerbPower) {
		t.Fatal("a machine with one power device is not offered power")
	}

	tree.waitFor(t, "/addon_power", "false")

	result, err := s.Do(t.Context(), connectorapi.Command{
		Verb: klipper.VerbPower, Args: map[string]string{"addon_power": "true"},
	})
	if err != nil || !result.OK {
		t.Fatalf("power on: %v %+v", err, result)
	}

	if host.PowerStatus("kp3s_plug") != "on" {
		t.Error("the device was not switched on")
	}

	tree.waitFor(t, "/addon_power", "true")

	none := connect(t, startMachine(t, klippersim.Options{Power: map[string]string{}}))
	if caps, _ = none.Capabilities(); caps.Has(klipper.VerbPower) {
		t.Error("a machine with no power device is offered power")
	}

	two := connect(t, startMachine(t, klippersim.Options{Power: map[string]string{"a": "on", "b": "on"}}))
	if caps, _ = two.Capabilities(); caps.Has(klipper.VerbPower) {
		t.Error("with two devices and none named, the driver guessed one")
	}
}

func TestAKlippyRestartEndsTheSession(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{})
	session := connect(t, host)

	host.Notify("notify_klippy_disconnected", nil)

	deadline := time.After(3 * time.Second)

	for {
		select {
		case _, open := <-session.State():
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("the session outlived Klipper")
		}
	}
}

func TestAStartingKlipperIsNotConnected(t *testing.T) {
	t.Parallel()

	host := startMachine(t, klippersim.Options{KlippyState: "startup"})
	driver := klipper.NewDriver(0)

	_, err := driver.Connect(t.Context(), connectorapi.Endpoint{Address: host.Address()}, driver.Baseline())
	if !errors.Is(err, klipper.ErrKlippyNotReady) {
		t.Errorf("connect answered %v", err)
	}
}

// --- helpers ---

func startMachine(t *testing.T, opts klippersim.Options) *klippersim.Host {
	t.Helper()

	host, err := klippersim.Start(t.Context(), opts)
	if err != nil {
		t.Fatalf("start a fake Moonraker: %v", err)
	}

	t.Cleanup(func() { _ = host.Close() })

	return host
}

type session interface {
	connectorapi.Session
	connectorapi.Filesystem
	connectorapi.FileManager
	connectorapi.Transfers
	FilesChanged() <-chan connectorapi.FileChange
}

func connect(t *testing.T, host *klippersim.Host) session {
	t.Helper()

	driver := klipper.NewDriver(0)

	s, err := driver.Connect(t.Context(), connectorapi.Endpoint{Address: host.Address()}, driver.Baseline())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	full, ok := s.(session)
	if !ok {
		t.Fatal("the session lacks the file interfaces")
	}

	return full
}

// tree collects the state patches of one session.
type tree struct {
	updates chan connectorapi.StatePatch
	values  map[string]string
}

func watch(t *testing.T, s connectorapi.Session) *tree {
	t.Helper()

	out := &tree{updates: make(chan connectorapi.StatePatch, 1024), values: map[string]string{}}

	go func() {
		for patch := range s.State() {
			out.updates <- patch
		}
	}()

	return out
}

func (tr *tree) get(path string) string {
	for {
		select {
		case patch := <-tr.updates:
			tr.values[patch.Path] = string(patch.Value)
		default:
			return tr.values[path]
		}
	}
}

func (tr *tree) waitFor(t *testing.T, path, want string) {
	t.Helper()

	if tr.values[path] == want {
		return
	}

	deadline := time.After(3 * time.Second)

	for {
		select {
		case patch := <-tr.updates:
			tr.values[patch.Path] = string(patch.Value)

			if patch.Path == path && compact(patch.Value) == want {
				return
			}
		case <-deadline:
			t.Fatalf("%s is %s, want %s", path, tr.values[path], want)
		}
	}
}

func compact(raw json.RawMessage) string {
	var out bytes.Buffer
	if json.Compact(&out, raw) != nil {
		return string(raw)
	}

	return out.String()
}

// memory is a connectorapi.Content held in memory.
type memory []byte

func (m memory) Size() int64 { return int64(len(m)) }

func (m memory) Open(_ context.Context, offset int64) (io.ReadCloser, error) {
	if offset > int64(len(m)) {
		offset = int64(len(m))
	}

	return io.NopCloser(bytes.NewReader(m[offset:])), nil
}
