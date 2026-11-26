# Klipper driver for Prusa Connect

A Prusa Connect printer driver for Klipper machines, reached through
[Moonraker](https://moonraker.readthedocs.io/). It covers Voron, Rat Rig,
converted Enders, Sovol, Elegoo, Qidi and any other printer that runs
Klipper with Moonraker (the API server behind Mainsail and Fluidd).

The server dials each printer (an outbound driver). One websocket carries
telemetry, commands and file operations. Uploads and thumbnails go over HTTP
to the same address.

## What it does

| area | support |
|---|---|
| telemetry | state, nozzle and bed temperatures and targets, chamber (`heater_generic chamber` or `temperature_sensor chamber`), position, speed and flow, tools, build volume, hostname |
| prints | start, pause, resume, cancel; job name, path, progress, time printing, time left (estimated), filament used, outcome |
| cancel objects | the object list from `exclude_object`, and `EXCLUDE_OBJECT` for one object |
| files | the `gcodes` folder as one storage: list, file details with the slicer's preview, print time, layer height and material, create and delete folders, delete files, live change events |
| uploads | streaming HTTP upload with progress, checked by listing the folder afterwards |
| controls | home, jog X/Y/Z, extrude/retract, disable motors, speed and flow, nozzle/bed/chamber targets, G-code console, bed mesh, load/unload filament macros, firmware restart, clear a finished print |
| errors | Klipper shutdown shows as `ERROR`. Reset printer (the red button under Controls → Maintenance) sends `FIRMWARE_RESTART`, which also does what Klipper's `RESTART` does. While Klipper restarts the printer shows `BUSY` and stays connected; if Moonraker itself restarts, the server dials it again |

The machine's own object list decides what is offered. For example, a printer
with no `[pause_resume]` is not offered pause, one with no `[bed_mesh]` is not
offered bed leveling, and load/unload need `LOAD_FILAMENT` / `UNLOAD_FILAMENT`
macros. The heater limits come from the printer's `max_temp`. The Prusa
profiles cap the nozzle at 300 °C for any printer they do not cover.

### Printer configuration Klipper needs

```ini
[virtual_sdcard]         # required to print files
path: ~/printer_data/gcodes
[pause_resume]           # pause, resume and cancel
[exclude_object]         # cancel single objects (enable "Label objects" in the slicer)
[display_status]         # progress from M73
```

## Important: no binary G-code

Klipper cannot print `.bgcode`. In PrusaSlicer, turn off **Printer Settings →
General → Firmware → Supports binary G-code** for the Klipper printer profile,
so it sends plain `.gcode`.

## Build a server with this driver

The SDK is not published yet, so the driver builds against a checkout of
prusa-connect placed beside this folder. You need Go 1.26.11 or later, make,
Node and pnpm:

```
prusa_connect/
├── prusa-connect/          # git clone https://github.com/prusa3d/prusa-connect
└── prusa-driver-klipper/   # git clone https://github.com/agreenbhm/prusa-driver-klipper
```

If the checkout is somewhere else, change the `replace` line in `go.mod` and
the paths in `build.yaml`.

```sh
cd prusa-driver-klipper && go test ./...                         # conformance suite + driver tests
cd ../prusa-connect
make embed-app                                                    # build the web app (needs Node and pnpm), once per checkout
make compose-build CONFIG=../prusa-driver-klipper/build.yaml      # → bin/server-klipper
```

## Run it in Docker (Compose or Swarm)

`deploy/Dockerfile` builds the published image with its server binary swapped
for one that has this driver compiled in. The bundled extensions, entry point,
health check and user stay as they are. It needs only this repository: the core
source is cloned at the release tag inside the build.

```sh
docker buildx build -f deploy/Dockerfile \
  --build-arg PRUSA_CONNECT_VERSION=0.1.0-beta.2 \
  --platform linux/amd64,linux/arm64 \
  -t <registry>/prusa-connect-klipper:0.1.0-beta.2 --push .
```

Then point the service's `image:` at that tag, add the `klipper` connector to
its `config.yaml` (below), and redeploy (`docker stack deploy -c stack.yml <stack>`).
Volumes, database and settings carry over.

- `PRUSA_CONNECT_VERSION` must be the version you already run. The core and the
  base image are both taken at that tag, and a newer core migrates the database
  forward with no way back.
- Upgrade by rebuilding with the new version. The in-app upgrade installs the
  stock binary, which has no Klipper driver.

## Configure it

Declare a connector in the server's `config.yaml` (under `core:`):

```yaml
core:
  connectors:
    klipper:
      type: klipper
      enabled: true
      config:
        timeout: 10s          # one call to the printer
        gcode_timeout: 5m     # one G-code command, such as homing or a bed mesh
        storage_name: usb     # the storage name; both PrusaSlicer connectors send to "usb"
        # api_key: ...        # only if Moonraker requires one; can also be set per printer
```

Then tell the server which printers to dial. Each printer is one endpoint:

```sh
curl -X PUT http://localhost:8080/api/v1/connector-endpoints/voron \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"connector":"klipper","address":"voron.local","enabled":true,
       "options":{"model":"Voron 2.4","unit_uid":"voron-2.4-serial-123"}}'
```

`address` can be:

- `voron.local` or `192.168.1.50`. This uses port 7125, Moonraker's own port.
- `voron.local:7125`.
- `http://voron.local/`. This goes through the nginx proxy that Mainsail and Fluidd install. Use `https://` for TLS, and add a path if Moonraker is behind a path prefix.

Endpoint `options`:

| option | meaning | default |
|---|---|---|
| `model` | model name shown for the printer | `Klipper` |
| `vendor` | maker | `Klipper` |
| `serial` | serial number | none |
| `unit_uid` | stable identity. Set it when the address can change | derived from the address |
| `api_key` | Moonraker API key for this printer | the connector's `api_key` |

Endpoint options can be read by anyone allowed to list endpoints. If that
matters, put the API key in the connector config instead, or let Moonraker
trust the server's address (`[authorization] trusted_clients`).

## Connector type name

Inside a server, the type is `klipper`. If another module also registers
`klipper`, use the qualified name `com.github.aux.agreenbhm.prusa-driver-klipper/klipper`.
The qualified name comes from the module path in `go.mod`.

## Simulator

`go run ./cmd/klipper-sim -addr 127.0.0.1:7125` runs a fake Moonraker. It has
a full machine and a small gcodes folder, so you can try the driver without a
printer.

## Known limits

- Klipper does not count jobs, so the job id is derived from the file name and
  the minute the print started. It stays the same if the server reconnects
  during a print.
- Time left is estimated from progress, the same way Mainsail and Fluidd do it.
- A print started from Mainsail, Fluidd or the printer's own screen is shown too.
- `force_gcode` (G-code outside the queue) is not offered. Moonraker runs every
  script in order.
- Uncancel object is not offered, because Klipper cannot include an excluded
  object again.
- Only the `gcodes` root is exposed, and hidden entries (`.thumbs`) are left out.
