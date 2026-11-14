package managedredis

import (
	"context"
	"regexp"
	"strings"
)

// These are the only scripts in the reviewed concord-ctl.sh operator source.
const cleanupRoomScript = `local removed = 0; if redis.call("GET", KEYS[1]) == ARGV[1] then removed = removed + redis.call("DEL", KEYS[1]) end; local live = redis.call("GET", KEYS[2]); if live and string.sub(live, 1, string.len(ARGV[1]) + 1) == ARGV[1] .. "-" then removed = removed + redis.call("DEL", KEYS[2]) end; return removed`

// resumePauseScript is media_legacy_resume_value: delete the join pause only
// while it still holds the value the operator read.
const resumePauseScript = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return -1`

// joinPauseKey is joinpause.Key; importing that package would pull gin into
// this binary.
const joinPauseKey = "media:legacy_join_pause"

// operatorPauseValue is joinpause's value shape: `<t0>:<deadline>[:<test-user-uuid>]`.
var operatorPauseValue = regexp.MustCompile(`^[0-9]{1,12}:[0-9]{1,12}(:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})?$`)

var operatorRoomID = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)
var operatorGeneration = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

type operatorExecutor interface {
	ExecuteOperator(context.Context, []string) (string, error)
}

func validColour(value string) bool { return value == "blue" || value == "green" }
func drainKey(value string) bool {
	return value == "media:colour_draining:blue" || value == "media:colour_draining:green"
}
func readableOperatorKey(value string) bool {
	return value == "media:active_colour" || value == joinPauseKey || drainKey(value)
}

func validRoomCleanup(args []string) bool {
	if len(args) != 6 || args[1] != cleanupRoomScript || args[2] != "2" || !validColour(args[5]) {
		return false
	}
	owner := strings.TrimPrefix(args[3], "voice:room_owner:")
	live := strings.TrimPrefix(args[4], "voice:room_live:")
	return strings.HasPrefix(args[3], "voice:room_owner:") && strings.HasPrefix(args[4], "voice:room_live:") && owner == live && operatorRoomID.MatchString(owner)
}

func validOperatorArgs(args []string) bool {
	if len(args) < 2 || len(args) > 6 {
		return false
	}
	for _, arg := range args {
		if arg == "" || len(arg) > 512 || strings.ContainsAny(arg, "\x00\r\n") {
			return false
		}
	}
	switch args[0] {
	case "GET", "EXISTS":
		return validOperatorRead(args)
	case "SET":
		return validOperatorSet(args)
	case "SADD", "SREM", "SISMEMBER":
		return validOperatorMembership(args)
	case "DEL":
		return len(args) == 2 && drainKey(args[1])
	case "--scan":
		return validOperatorScan(args)
	case "EVAL":
		return validRoomCleanup(args) || validPauseResume(args)
	default:
		return false
	}
}

func validOperatorRead(args []string) bool {
	return len(args) == 2 && readableOperatorKey(args[1])
}

// validPauseSet admits the two writes media_legacy_pause makes: arm a new pause
// (NX) and add a test user to the existing one (XX).
func validPauseSet(args []string) bool {
	return len(args) == 4 && args[1] == joinPauseKey && operatorPauseValue.MatchString(args[2]) &&
		(args[3] == "NX" || args[3] == "XX")
}

func validPauseResume(args []string) bool {
	return len(args) == 5 && args[1] == resumePauseScript && args[2] == "1" && args[3] == joinPauseKey &&
		operatorPauseValue.MatchString(args[4])
}

func validOperatorSet(args []string) bool {
	if validPauseSet(args) {
		return true
	}
	if len(args) != 3 {
		return false
	}
	return args[1] == "media:active_colour" && validColour(args[2]) ||
		drainKey(args[1]) && operatorGeneration.MatchString(args[2])
}

func validOperatorMembership(args []string) bool {
	return len(args) == 3 && args[1] == "media:colours:registered" && validColour(args[2])
}

func validOperatorScan(args []string) bool {
	return len(args) == 3 && args[1] == "--pattern" &&
		(args[2] == "voice:room_owner:*" || args[2] == "voice:room_live:*" || args[2] == "media:colour_draining:*")
}

// ExecuteOperator accepts only reviewed source calls, then delegates to the
// controller's Redis admin connection. It never interprets caller Lua or URLs.
func (c *Controller) ExecuteOperator(ctx context.Context, args []string) (string, error) {
	if !validOperatorArgs(args) {
		return "", errController
	}
	c.mu.Lock()
	loaded := c.loaded && c.failureCode == ""
	c.mu.Unlock()
	if !loaded {
		return "", errController
	}
	admin, ok := c.admin.(operatorExecutor)
	if !ok {
		return "", errController
	}
	output, err := admin.ExecuteOperator(ctx, args)
	if err != nil || len(output) > 16*1024 {
		return "", errController
	}
	return output, nil
}
