// Package telebot turns the alerting bot into a control panel: menus, buttons,
// an allow list, and a confirmation step in front of anything destructive.
package telebot

import (
	"fmt"
	"strconv"
	"strings"
)

// Action codes are deliberately two characters. Telegram caps callback_data at
// 64 bytes, and the address never goes in there — it would not always fit, and
// the payload is visible to the client anyway.
const (
	ActMenu       = "mm" // main menu
	ActServerList = "sl" // paginated endpoint list, arg = page
	ActServer     = "sv" // one endpoint, arg = target id
	ActBlocked    = "bl" // only the blocked endpoints
	ActProviders  = "pv" // ownership summary
	ActHistory    = "ph" // provisioning history
	ActScan       = "sc" // start a full scan
	ActSync       = "up" // re-read the panel and probe what is new
	ActStatus     = "st" // service status

	ActNotifyOnly  = "no" // toggle hands-off, arg = target id
	ActRequestNew  = "rq" // request a replacement, arg = target id
	ActNodeDisable = "nx" // disable the node in 3x-ui, arg = target id
	ActNodeEnable  = "ne" // enable it again, arg = target id
	ActNodeDelete  = "dn" // delete the node, arg = target id
	ActHetznerDel  = "dh" // destroy the Hetzner server, arg = target id
	ActHide        = "hd" // stop serving it to users, arg = target id
	ActShow        = "sh" // serve it to users again, arg = target id
	// ActSwapNow starts a full replacement by hand: hide the configs, buy a
	// server, prove it works from Iran, move the configs across. It waives the
	// cooldown and the fully-blocked rule, which exist to stop the service
	// acting on its own too eagerly — a person pressing this has already looked.
	ActSwapNow = "sw" // replace this machine now, arg = target id

	// The ledger of addresses this service destroyed, so they are never bought
	// back. The address itself is the argument: it is at most 15 characters, so
	// it fits Telegram's 64-byte limit comfortably, and it is already visible to
	// anyone who can see the screen.
	ActLedger     = "lg" // list the barred addresses
	ActLedgerItem = "li" // one entry, arg = address
	ActRelease    = "lr" // let an address be used again, arg = address

	ActConfirm = "ok" // run a pending action, arg = token
	ActCancel  = "cx" // drop a pending action, arg = token
	ActNoop    = "np" // a label that does nothing
)

const callbackVersion = "1"

// Callback is a decoded button payload.
type Callback struct {
	Action string
	Arg    string
}

// ID reads the argument as a numeric id.
func (c Callback) ID() int64 {
	n, _ := strconv.ParseInt(c.Arg, 10, 64)
	return n
}

// Page reads the argument as a page number, defaulting to the first.
func (c Callback) Page() int {
	n, err := strconv.Atoi(c.Arg)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// Encode builds a callback payload. The result is always far below Telegram's
// 64-byte limit: a two-character action plus a numeric id.
func Encode(action string, arg any) string {
	switch v := arg.(type) {
	case nil:
		return callbackVersion + "|" + action + "|"
	case string:
		return callbackVersion + "|" + action + "|" + v
	case int:
		return callbackVersion + "|" + action + "|" + strconv.Itoa(v)
	case int64:
		return callbackVersion + "|" + action + "|" + strconv.FormatInt(v, 10)
	default:
		return callbackVersion + "|" + action + "|" + fmt.Sprint(v)
	}
}

// Decode parses a callback payload. An unknown version is rejected so that a
// button left over from an older release cannot be misread as something else.
func Decode(data string) (Callback, error) {
	parts := strings.SplitN(data, "|", 3)
	if len(parts) != 3 {
		return Callback{}, fmt.Errorf("malformed callback data")
	}
	if parts[0] != callbackVersion {
		return Callback{}, fmt.Errorf("callback from an older version")
	}
	if parts[1] == "" {
		return Callback{}, fmt.Errorf("callback has no action")
	}
	return Callback{Action: parts[1], Arg: parts[2]}, nil
}
