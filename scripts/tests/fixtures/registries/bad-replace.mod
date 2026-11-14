module example.invalid/registries-bad-replace

go 1.26

require (
	example.invalid/forked v1.0.0
	example.invalid/vendored v1.0.0
)

// A module-path replacement is fetched through the module proxy: allowed.
replace example.invalid/forked => example.invalid/forked-fixed v1.0.1

// A filesystem replacement bypasses the proxy and is not listed: must fail.
replace example.invalid/vendored => ../../../../vendored-copy
