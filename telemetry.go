package klipper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
)

// Where a Klipper reading lands in the state tree. The paths are the ones the
// Prusa driver writes, so one client reads a Prusa and a Klipper machine at the
// same addresses.
const (
	statePath         = "/state"
	jobNamePath       = "/job/name"
	bedPath           = "/temperature/bed"
	bedTargetPath     = "/temperature/bed_target"
	chamberPath       = "/temperature/chamber"
	chamberTargetPath = "/temperature/chamber_target"
	speedPath         = "/speed"
	flowPath          = "/flow"
	hostnamePath      = "/network/hostname"
	stateMessagePath  = "/state_message"
)

// jobOutcomes maps Klipper's own terminal words onto the closed set the host
// publishes. A word this map does not carry writes no outcome, and the host
// then publishes the print as unknown.
//
//nolint:gochecknoglobals // a lookup table, read-only after init.
var jobOutcomes = map[string]connectorapi.JobOutcome{
	"complete":  connectorapi.JobFinished,
	"cancelled": connectorapi.JobCancelled,
	"error":     connectorapi.JobFailed,
}

// klipperPrintingStates are Klipper's words for a print that is on the bed.
//
//nolint:gochecknoglobals // a fixed set, read-only after init.
var klipperPrintingStates = []string{"printing", "paused"}

// axisPaths is where the three axes of toolhead.position land.
//
//nolint:gochecknoglobals // a fixed list, read-only after init.
var axisPaths = []string{"/axis/x", "/axis/y", "/axis/z"}

// printWords maps Klipper's print_stats.state onto the words the other drivers
// report. A word this map does not carry reaches the tree in upper case.
//
//nolint:gochecknoglobals // a lookup table, read-only after init.
var printWords = map[string]connectorapi.StateName{
	"standby":   stateIdle,
	"printing":  statePrinting,
	"paused":    statePaused,
	"complete":  stateFinished,
	"cancelled": stateStopped,
	"error":     stateError,
}

// percent is the scale a Klipper fraction is reported on.
const percent = 100

// roundingUnit is how far a converted number is rounded, as the reciprocal of
// the step.
const roundingUnit = 100

// jobStartRounding is how coarsely a derived start is rounded, so a reconnect
// mid-print derives the same id.
const jobStartRounding = time.Minute

// jobIDBytes is how much of the digest a derived id carries.
const jobIDBytes = 16

// minProgressForEstimate is how far a print must be before the time left is
// estimated from it. Earlier, heating and homing make the estimate wild.
const minProgressForEstimate = 0.01

// reader is what this driver remembers between two readings of one machine.
// Moonraker sends only what changed, so a reading alone does not say which
// print is on the bed or which head is printing.
//
// Only the session's reader goroutine calls patchesFrom. objects is also read
// by Do, so it has its own lock.
type reader struct {
	storage string
	chamber string

	heads heads

	// klippy is webhooks.state: ready, startup, shutdown or error.
	klippy string
	// printing is print_stats.state.
	printing string
	// written is the last word written at /state.
	written connectorapi.StateName

	// The print on the bed now, empty when nothing is printing.
	jobID   string
	jobFile string

	duration float64
	progress float64

	objectsMu sync.Mutex
	objects   []string
	// lastExcluded is the excluded object names last reported. Only the
	// reader goroutine touches it.
	lastExcluded []string
}

// objectName is the name of the object with the given id in the running
// print, for cancel_object.
func (r *reader) objectName(id int) (string, bool) {
	r.objectsMu.Lock()
	defer r.objectsMu.Unlock()

	if id < 0 || id >= len(r.objects) {
		return "", false
	}

	return r.objects[id], true
}

// patchesFrom turns one reading of the Klipper objects into patches, in a fixed
// order. A field the machine did not report produces no patch.
func (r *reader) patchesFrom(status map[string]json.RawMessage, now time.Time) []connectorapi.StatePatch {
	objects := objectFields(status)

	var out []connectorapi.StatePatch

	hooks := objects[objectWebhooks]
	if word := klipperText(hooks["state"]); word != "" {
		r.klippy = word
		out = appendVerbatim(out, hooks["state_message"], stateMessagePath)
	}

	stats := objects[objectPrintStats]
	if word := klipperText(stats["state"]); word != "" {
		r.printing = word
	}

	if word := r.stateWord(); word != "" && word != r.written {
		r.written = word
		out = appendText(out, statePath, string(word))
	}

	before := r.jobID
	out = r.appendJob(out, stats, now)

	if before != "" || r.jobID != "" {
		out = appendVerbatim(out, stats["filename"], jobNamePath)

		if file := klipperText(stats["filename"]); file != "" {
			out = appendText(out, connectorapi.JobPathPath, "/"+r.storage+"/"+file)
		}

		out = appendVerbatim(out, stats["print_duration"], connectorapi.JobTimePrintingPath)
		out = appendVerbatim(out, stats["filament_used"], connectorapi.JobFilamentUsedPath)
		out = r.appendProgress(out, stats, objects)
	}

	out = r.appendObjects(out, objects[objectExcludeObject], before == "" && r.jobID != "")
	out = r.heads.appendReadings(out, objects)

	bed := objects[objectHeaterBed]
	out = appendVerbatim(out, bed["temperature"], bedPath)
	out = appendVerbatim(out, bed["target"], bedTargetPath)

	if r.chamber != "" {
		chamber := objects[r.chamber]
		out = appendVerbatim(out, chamber["temperature"], chamberPath)
		out = appendVerbatim(out, chamber["target"], chamberTargetPath)
	}

	out = appendPosition(out, objects[objectToolhead]["position"])

	move := objects[objectGcodeMove]
	out = appendScaled(out, move["speed_factor"], speedPath)
	out = appendScaled(out, move["extrude_factor"], flowPath)

	return out
}

// stateWord is the word /state carries now. Klipper that is shut down or
// starting outranks whatever print_stats last said.
func (r *reader) stateWord() connectorapi.StateName {
	switch r.klippy {
	case "shutdown", "error":
		return stateError
	case "startup":
		return stateBusy
	}

	if r.printing == "" {
		return ""
	}

	if word, known := printWords[r.printing]; known {
		return word
	}

	return connectorapi.StateName(strings.ToUpper(r.printing))
}

// appendJob gives the print an id and, when it ends, an outcome.
//
// The id is minted on the first reading that says a file is printing, and held
// until the machine prints something else.
func (r *reader) appendJob(
	out []connectorapi.StatePatch, stats map[string]json.RawMessage, now time.Time,
) []connectorapi.StatePatch {
	state, file := klipperText(stats["state"]), klipperText(stats["filename"])

	if state == "" && file == "" {
		return out
	}

	if state == "" {
		state = r.printing
	}

	if file == "" {
		file = r.jobFile
	}

	if !slices.Contains(klipperPrintingStates, state) {
		if r.jobID == "" {
			return out
		}

		if outcome, ended := jobOutcomes[state]; ended {
			out = appendText(out, connectorapi.JobOutcomePath, outcome)

			if outcome == connectorapi.JobFinished {
				out = append(out,
					connectorapi.StatePatch{Path: connectorapi.JobProgressPath, Value: json.RawMessage("100")},
					connectorapi.StatePatch{Path: connectorapi.JobTimeRemainingPath, Value: json.RawMessage("0")})
			}
		}

		r.jobID, r.jobFile, r.duration, r.progress = "", "", 0, 0

		return out
	}

	if r.jobID == "" || r.jobFile != file {
		r.jobFile, r.jobID = file, derivedJobID(file, startOf(stats, now))
		r.duration, r.progress = 0, 0
	}

	return appendText(out, connectorapi.JobIDPath, r.jobID)
}

// appendProgress writes how far the print is and how long is left. Klipper
// reports no time left, so it is estimated from the time printed so far and
// the share of the file done, as Mainsail and Fluidd estimate it.
func (r *reader) appendProgress(
	out []connectorapi.StatePatch, stats map[string]json.RawMessage, objects map[string]map[string]json.RawMessage,
) []connectorapi.StatePatch {
	if r.jobID == "" {
		return out
	}

	changed := false

	if duration, ok := klipperNumber(stats["print_duration"]); ok {
		r.duration, changed = duration, true
	}

	raw := objects[objectDisplayStatus]["progress"]
	if len(raw) == 0 {
		raw = objects[objectVirtualSD]["progress"]
	}

	if progress, ok := klipperNumber(raw); ok {
		r.progress, changed = progress, true
		out = appendScaled(out, raw, connectorapi.JobProgressPath)
	}

	if !changed || r.progress < minProgressForEstimate || r.duration <= 0 {
		return out
	}

	left := math.Max(0, math.Round(r.duration/r.progress-r.duration))

	return append(out, connectorapi.StatePatch{
		Path: connectorapi.JobTimeRemainingPath, Value: json.RawMessage(strconv.FormatFloat(left, 'f', 0, 64)),
	})
}

// excludeObject is what Klipper's exclude_object section reports.
type excludeObject struct {
	Objects []struct {
		Name string `json:"name"`
	} `json:"objects"`
	Excluded []string `json:"excluded_objects"`
}

// appendObjects writes the objects of the print that can be cancelled one by
// one. Klipper names them, and the id is the place in its list.
//
// started is true on the reading that began a print. Klipper defines the
// objects at the top of the file, which can arrive before print_stats says the
// print began, so the list held is written then.
func (r *reader) appendObjects(
	out []connectorapi.StatePatch, fields map[string]json.RawMessage, started bool,
) []connectorapi.StatePatch {
	if len(fields) == 0 {
		if !started || len(r.objects) == 0 {
			return out
		}

		fields = map[string]json.RawMessage{}
	}

	raw, err := json.Marshal(fields)
	if err != nil {
		return out
	}

	var reported excludeObject
	if json.Unmarshal(raw, &reported) != nil {
		return out
	}

	r.objectsMu.Lock()

	if _, sent := fields["objects"]; sent {
		r.objects = r.objects[:0]
		for _, object := range reported.Objects {
			r.objects = append(r.objects, object.Name)
		}
	}

	names := slices.Clone(r.objects)
	r.objectsMu.Unlock()

	// Moonraker sends only what changed, so a reading with one of the two
	// fields needs the other one remembered.
	excluded := reported.Excluded
	if _, sent := fields["excluded_objects"]; !sent {
		excluded = r.lastExcluded
	} else {
		r.lastExcluded = slices.Clone(excluded)
	}

	list := make([]connectorapi.JobObject, 0, len(names))
	for id, name := range names {
		list = append(list, connectorapi.JobObject{ID: id, Name: name, Cancelled: slices.Contains(excluded, name)})
	}

	// The objects belong to a print, and a print is named by its id, so an
	// idle machine's empty list is remembered and not written.
	if r.jobID == "" {
		return out
	}

	encoded, err := json.Marshal(list)
	if err != nil {
		return out
	}

	return append(out, connectorapi.StatePatch{Path: connectorapi.JobObjectsPath, Value: encoded})
}

// startOf is when this print began, from the machine's own count of how long it
// has been running.
func startOf(stats map[string]json.RawMessage, now time.Time) time.Time {
	running, ok := klipperNumber(stats["total_duration"])
	if !ok {
		return now.Truncate(jobStartRounding)
	}

	return now.Add(-time.Duration(running * float64(time.Second))).Truncate(jobStartRounding)
}

// derivedJobID is the id this driver gives a print the machine did not number:
// the file and the minute the print began, as the hex of their digest.
func derivedJobID(file string, started time.Time) string {
	sum := sha256.Sum256([]byte(file + "\x00" + started.UTC().Format(time.RFC3339)))

	return hex.EncodeToString(sum[:jobIDBytes])
}

// klipperText reads a reported value as text, and answers empty for anything
// else.
func klipperText(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}

	return value
}

// klipperNumber reads a reported value as a number.
func klipperNumber(raw json.RawMessage) (float64, bool) {
	var value float64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}

	return value, true
}

// appendText writes one string value at a path.
func appendText(out []connectorapi.StatePatch, path, value string) []connectorapi.StatePatch {
	encoded, err := json.Marshal(value)
	if err != nil {
		return out
	}

	return append(out, connectorapi.StatePatch{Path: path, Value: encoded})
}

// objectFields reads each object's fields without reading their values, so a
// value passed through reaches the host as the machine encoded it.
func objectFields(status map[string]json.RawMessage) map[string]map[string]json.RawMessage {
	out := make(map[string]map[string]json.RawMessage, len(status))

	for object, raw := range status {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}

		out[object] = fields
	}

	return out
}

// appendVerbatim passes a value on exactly as the machine encoded it. A null is
// skipped, because a null removes a path.
func appendVerbatim(out []connectorapi.StatePatch, raw json.RawMessage, path string) []connectorapi.StatePatch {
	if len(raw) == 0 || string(raw) == "null" {
		return out
	}

	return append(out, connectorapi.StatePatch{Path: path, Value: raw})
}

// appendScaled converts a Klipper fraction to the percentage its path carries.
func appendScaled(out []connectorapi.StatePatch, raw json.RawMessage, path string) []connectorapi.StatePatch {
	fraction, ok := klipperNumber(raw)
	if !ok {
		return out
	}

	rounded := math.Round(fraction*percent*roundingUnit) / roundingUnit
	encoded := strconv.FormatFloat(rounded, 'f', -1, 64)

	return append(out, connectorapi.StatePatch{Path: path, Value: json.RawMessage(encoded)})
}

// appendPosition writes the three axes Klipper reports. The fourth element is
// the extruder's own position, which is not an axis a client shows.
func appendPosition(out []connectorapi.StatePatch, raw json.RawMessage) []connectorapi.StatePatch {
	if len(raw) == 0 {
		return out
	}

	var position []json.RawMessage
	if json.Unmarshal(raw, &position) != nil {
		return out
	}

	for index, path := range axisPaths {
		if index >= len(position) {
			break
		}

		out = append(out, connectorapi.StatePatch{Path: path, Value: position[index]})
	}

	return out
}
