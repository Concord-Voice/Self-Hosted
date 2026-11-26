package stepup

import "github.com/redis/go-redis/v9"

// ChargeCommandKey returns the counter key a budget script call names (a
// charge, or a refund), or "".
// A budget charge is one script (ChargeKey), so a test that injects a fault
// into the charge matches the script call by its first key rather than an
// INCR that no longer runs on its own.
func ChargeCommandKey(cmd redis.Cmder) string {
	if cmd.Name() != "evalsha" && cmd.Name() != "eval" {
		return ""
	}
	args := cmd.Args()
	if len(args) < 4 {
		return ""
	}
	key, _ := args[3].(string)
	return key
}
