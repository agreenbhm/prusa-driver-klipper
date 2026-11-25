// A Prusa Connect driver for Klipper printers, reached through Moonraker.
//
// It is built against a checkout of prusa-connect, because the sdk module is
// not published yet. The replace below points at that checkout. Change it if
// your checkout is somewhere else.
module example.com/prusa-driver-klipper

go 1.26.11

require (
	github.com/gorilla/websocket v1.5.3
	github.com/prusa3d/prusa-connect/sdk v0.0.0
)

replace github.com/prusa3d/prusa-connect/sdk => ../prusa-connect/sdk
